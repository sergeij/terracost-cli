package main

// SQLite implementation of terracost's backend.Backend.
// Mirrors github.com/cycloidio/terracost/mysql but speaks SQLite-flavored SQL:
// ON CONFLICT … DO UPDATE … RETURNING id (instead of MySQL's ON DUPLICATE KEY +
// LAST_INSERT_ID trick), json_extract from JSON1 (no JSON_UNQUOTE needed), and a
// REGEXP scalar function registered against modernc.org/sqlite to substitute
// for MySQL's RLIKE.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/lopezator/migrator"
	"github.com/shopspring/decimal"
	"golang.org/x/text/currency"
	msqlite "modernc.org/sqlite"

	"github.com/cycloidio/terracost/price"
	"github.com/cycloidio/terracost/product"
)

var regexpCache sync.Map

func init() {
	msqlite.MustRegisterDeterministicScalarFunction(
		"regexp", 2,
		func(_ *msqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			pat, _ := args[0].(string)
			val, _ := args[1].(string)
			cached, ok := regexpCache.Load(pat)
			if !ok {
				re, err := regexp.Compile(pat)
				if err != nil {
					return nil, err
				}
				regexpCache.Store(pat, re)
				cached = re
			}
			return cached.(*regexp.Regexp).MatchString(val), nil
		},
	)
}

const schema = `
CREATE TABLE IF NOT EXISTS pricing_products (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    provider TEXT NOT NULL,
    sku TEXT NOT NULL,
    location TEXT NOT NULL,
    service TEXT NOT NULL,
    family TEXT NOT NULL DEFAULT '',
    attributes TEXT NOT NULL,
    UNIQUE (provider, sku, location)
);

CREATE INDEX IF NOT EXISTS idx__provider__location__service__family
    ON pricing_products (provider, location, service, family);

CREATE TABLE IF NOT EXISTS pricing_product_prices (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    product_id INTEGER NOT NULL,
    hash TEXT NOT NULL,
    currency TEXT NOT NULL,
    unit TEXT NOT NULL,
    price TEXT NOT NULL,
    attributes TEXT NOT NULL,
    UNIQUE (product_id, hash),
    FOREIGN KEY (product_id) REFERENCES pricing_products (id)
);
`

const schemaMeta = `
CREATE TABLE IF NOT EXISTS terracost_meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

// Migrate creates the schema if missing. SQLite doesn't need the MySQL evolution
// chain (rename indexes / widen unit) since we declare the final shape from the
// start; new tables get added as separate migrations so existing DBs pick them up.
func Migrate(ctx context.Context, db *sql.DB, table string) error {
	mig, err := migrator.New(
		migrator.TableName(table),
		migrator.Migrations(
			&migrator.Migration{
				Name: "Initial",
				Func: func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, schema)
					return err
				},
			},
			&migrator.Migration{
				Name: "Meta",
				Func: func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, schemaMeta)
					return err
				},
			},
		),
	)
	if err != nil {
		return err
	}
	return mig.Migrate(db)
}

// GetMeta returns the value for key, or "" with a nil error if no row exists.
func GetMeta(ctx context.Context, db *sql.DB, key string) (string, error) {
	var value string
	err := db.QueryRowContext(ctx, "SELECT value FROM terracost_meta WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return value, nil
}

// SetMeta upserts key=value.
func SetMeta(ctx context.Context, db *sql.DB, key, value string) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO terracost_meta (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value
	`, key, value)
	return err
}

type querier interface {
	QueryContext(ctx context.Context, q string, args ...interface{}) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...interface{}) *sql.Row
	ExecContext(ctx context.Context, q string, args ...interface{}) (sql.Result, error)
}

// Backend implements terracost/backend.Backend on top of a *sql.DB opened with the
// "sqlite" driver (modernc.org/sqlite).
type Backend struct {
	productRepo *ProductRepository
	priceRepo   *PriceRepository
}

func NewBackend(db *sql.DB) *Backend {
	return &Backend{
		productRepo: NewProductRepository(db),
		priceRepo:   NewPriceRepository(db),
	}
}

func (b *Backend) Products() product.Repository { return b.productRepo }
func (b *Backend) Prices() price.Repository     { return b.priceRepo }

// ---- ProductRepository ----

type ProductRepository struct {
	q querier
}

func NewProductRepository(q querier) *ProductRepository { return &ProductRepository{q: q} }

type dbProduct struct {
	ID         product.ID
	SKU        string
	Provider   string
	Service    string
	Family     string
	Location   string
	Attributes string
}

func (p *dbProduct) toDomainEntity() *product.Product {
	var attrs map[string]string
	_ = json.Unmarshal([]byte(p.Attributes), &attrs)
	return &product.Product{
		ID:         p.ID,
		SKU:        p.SKU,
		Provider:   p.Provider,
		Service:    p.Service,
		Family:     p.Family,
		Location:   p.Location,
		Attributes: attrs,
	}
}

func newProduct(p *product.Product) (*dbProduct, error) {
	attrs, err := json.Marshal(p.Attributes)
	if err != nil {
		return nil, err
	}
	return &dbProduct{
		SKU:        p.SKU,
		Provider:   p.Provider,
		Service:    p.Service,
		Family:     p.Family,
		Location:   p.Location,
		Attributes: string(attrs),
	}, nil
}

func (r *ProductRepository) Filter(ctx context.Context, filter *product.Filter) ([]*product.Product, error) {
	w := parseProductFilter(filter)
	q := fmt.Sprintf(`
		SELECT id, provider, sku, service, family, location, attributes
		FROM pricing_products
		WHERE %s
	`, w.String())

	rows, err := r.q.QueryContext(ctx, q, w.Parameters()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ps := make([]*product.Product, 0)
	for rows.Next() {
		p := &dbProduct{}
		if err := rows.Scan(&p.ID, &p.Provider, &p.SKU, &p.Service, &p.Family, &p.Location, &p.Attributes); err != nil {
			return nil, err
		}
		ps = append(ps, p.toDomainEntity())
	}
	return ps, rows.Err()
}

func (r *ProductRepository) FindByVendorAndSKU(ctx context.Context, vendor, sku string) (*product.Product, error) {
	q := `
		SELECT id, provider, sku, service, family, location, attributes
		FROM pricing_products
		WHERE provider = ? AND sku = ?
		LIMIT 1
	`
	row := r.q.QueryRowContext(ctx, q, vendor, sku)
	p := &dbProduct{}
	if err := row.Scan(&p.ID, &p.Provider, &p.SKU, &p.Service, &p.Family, &p.Location, &p.Attributes); err != nil {
		return nil, err
	}
	return p.toDomainEntity(), nil
}

func (r *ProductRepository) Upsert(ctx context.Context, prod *product.Product) (product.ID, error) {
	p, err := newProduct(prod)
	if err != nil {
		return 0, err
	}
	q := `
		INSERT INTO pricing_products (provider, sku, service, family, location, attributes)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (provider, sku, location) DO UPDATE SET
			attributes = excluded.attributes
		RETURNING id
	`
	var id int64
	err = r.q.QueryRowContext(ctx, q, p.Provider, p.SKU, p.Service, p.Family, p.Location, p.Attributes).Scan(&id)
	if err != nil {
		return 0, err
	}
	return product.ID(id), nil
}

// ---- PriceRepository ----

type PriceRepository struct {
	q querier
}

func NewPriceRepository(q querier) *PriceRepository { return &PriceRepository{q: q} }

type dbPrice struct {
	ID         price.ID
	ProductID  product.ID
	Hash       string
	Currency   string
	Value      decimal.Decimal
	Unit       string
	Attributes string
}

func (p *dbPrice) toDomainEntity() *price.Price {
	var attrs map[string]string
	_ = json.Unmarshal([]byte(p.Attributes), &attrs)
	return &price.Price{
		ID:         p.ID,
		Currency:   p.Currency,
		Value:      p.Value,
		Unit:       p.Unit,
		Attributes: attrs,
	}
}

func newPrice(pwp *price.WithProduct) (*dbPrice, error) {
	attrs, err := json.Marshal(pwp.Attributes)
	if err != nil {
		return nil, err
	}
	cur, err := currency.ParseISO(pwp.Currency)
	if err != nil {
		return nil, err
	}
	return &dbPrice{
		ProductID:  pwp.Product.ID,
		Hash:       pwp.GenerateHash(),
		Currency:   cur.String(),
		Value:      pwp.Value,
		Unit:       pwp.Unit,
		Attributes: string(attrs),
	}, nil
}

func (r *PriceRepository) Filter(ctx context.Context, productID product.ID, filter *price.Filter) ([]*price.Price, error) {
	w := parsePriceFilter(filter, productID)
	q := fmt.Sprintf(`
		SELECT id, hash, product_id, currency, price, unit, attributes
		FROM pricing_product_prices
		WHERE %s
	`, w.String())

	rows, err := r.q.QueryContext(ctx, q, w.Parameters()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ps := make([]*price.Price, 0)
	for rows.Next() {
		p := &dbPrice{}
		if err := rows.Scan(&p.ID, &p.Hash, &p.ProductID, &p.Currency, &p.Value, &p.Unit, &p.Attributes); err != nil {
			return nil, err
		}
		ps = append(ps, p.toDomainEntity())
	}
	return ps, rows.Err()
}

func (r *PriceRepository) Upsert(ctx context.Context, pwp *price.WithProduct) (price.ID, error) {
	p, err := newPrice(pwp)
	if err != nil {
		return 0, err
	}
	q := `
		INSERT INTO pricing_product_prices (product_id, hash, currency, price, unit, attributes)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (product_id, hash) DO UPDATE SET
			currency = excluded.currency,
			price = excluded.price,
			unit = excluded.unit,
			attributes = excluded.attributes
		RETURNING id
	`
	var id int64
	err = r.q.QueryRowContext(ctx, q, p.ProductID, p.Hash, p.Currency, p.Value, p.Unit, p.Attributes).Scan(&id)
	if err != nil {
		return 0, err
	}
	return price.ID(id), nil
}

func (r *PriceRepository) DeleteByProductWithKeep(ctx context.Context, productID product.ID, keep []price.ID) error {
	marks := make([]string, 0, len(keep))
	values := make([]interface{}, 0, len(keep)+1)
	values = append(values, productID)
	for _, v := range keep {
		marks = append(marks, "?")
		values = append(values, v)
	}
	q := fmt.Sprintf(`DELETE FROM pricing_product_prices WHERE product_id = ? AND id NOT IN (%s)`, strings.Join(marks, ","))
	_, err := r.q.ExecContext(ctx, q, values...)
	return err
}

// ---- Filter parsing ----

type Where struct {
	conditions []string
	params     []interface{}
}

func (w *Where) String() string {
	if len(w.conditions) == 0 {
		return "1=1"
	}
	return strings.Join(w.conditions, " AND ")
}

func (w *Where) Parameters() []interface{} { return w.params }

func (w *Where) add(c string, p ...interface{}) {
	w.conditions = append(w.conditions, c)
	w.params = append(w.params, p...)
}

func parseProductFilter(filter *product.Filter) *Where {
	w := &Where{}
	if filter == nil {
		return w
	}
	type fm struct {
		key string
		val *string
	}
	for _, f := range []fm{
		{"provider", filter.Provider},
		{"location", filter.Location},
		{"service", filter.Service},
		{"family", filter.Family},
		{"sku", filter.SKU},
	} {
		if f.val != nil {
			w.add(fmt.Sprintf("%s = ?", f.key), *f.val)
		}
	}
	for _, f := range filter.AttributeFilters {
		if f.Value != nil {
			w.add(fmt.Sprintf("json_extract(attributes, '$.%s') = ?", f.Key), *f.Value)
		} else if f.ValueRegex != nil {
			w.add(fmt.Sprintf("json_extract(attributes, '$.%s') REGEXP ?", f.Key), *f.ValueRegex)
		}
	}
	return w
}

func parsePriceFilter(filter *price.Filter, productID product.ID) *Where {
	w := &Where{}
	if productID != 0 {
		w.add("product_id = ?", productID)
	}
	if filter == nil {
		return w
	}
	if filter.Unit != nil {
		w.add("unit = ?", *filter.Unit)
	}
	if filter.Currency != nil {
		w.add("currency = ?", *filter.Currency)
	}
	for _, f := range filter.AttributeFilters {
		if f.Value != nil {
			w.add(fmt.Sprintf("json_extract(attributes, '$.%s') = ?", f.Key), *f.Value)
		} else if f.ValueRegex != nil {
			w.add(fmt.Sprintf("json_extract(attributes, '$.%s') REGEXP ?", f.Key), *f.ValueRegex)
		}
	}
	return w
}
