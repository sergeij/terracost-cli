package main

import (
	"bytes"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/cycloidio/terracost"
	"github.com/cycloidio/terracost/aws"
	"github.com/cycloidio/terracost/backend"
	"github.com/cycloidio/terracost/cost"
	"github.com/cycloidio/terracost/usage"
	"github.com/shopspring/decimal"
)

func helpUsage() {
	fmt.Fprint(os.Stderr, "Terracost\n\n")
	flag.PrintDefaults()
	os.Exit(2)
}

var (
	flagIngest        bool
	flagIngestMinimal bool   = true
	flagIngestRegion  string = "us-east-1"
	flagestimatePlan  string = ""
	flagProvider      string = "aws"
	flagPRComment     bool
	flagSqlitePath    string = "./terracost.db"
	flagLazy          bool
	flagLazyMaxAge    time.Duration
)

func main() {

	flag.Usage = helpUsage
	flag.BoolVar(&flagIngest, "ingest", flagIngest, "Run price ingester")
	flag.BoolVar(&flagIngestMinimal, "ingest-minimal", flagIngestMinimal, "Minimal ingest")
	flag.StringVar(&flagIngestRegion, "ingest-region", flagIngestRegion, "Region used to ingest")
	flag.StringVar(&flagestimatePlan, "estimate-plan", flagestimatePlan, "terraform-plan.json file path to estimate (example: ./terraform-plan.json)")
	flag.StringVar(&flagProvider, "provider", flagProvider, "Terraform provider used [aws]")
	flag.BoolVar(&flagPRComment, "pr-comment", flagPRComment, "Post (or update) a cost summary comment on a GitHub PR (requires GH_TOKEN or GITHUB_APP_* env vars; GITHUB_APP_PEM_FILE accepts a path or the PEM inline; needs BASE_REPO_OWNER, BASE_REPO_NAME, PULL_NUM)")
	flag.StringVar(&flagSqlitePath, "sqlite-path", flagSqlitePath, "Path to the SQLite database file")
	flag.BoolVar(&flagLazy, "lazy", flagLazy, "With -ingest, skip if a successful prior ingest exists for the same region")
	flag.DurationVar(&flagLazyMaxAge, "lazy-max-age", flagLazyMaxAge, "With -lazy, only skip if the prior ingest is within this age (e.g. 168h for 7 days; 0 = no age limit)")

	flag.Parse()

	// get command line args
	args := flag.Args()
	if len(args) == 0 {
	}

	if !flagIngest && flagestimatePlan == "" {
		helpUsage()
		os.Exit(0)
	}

	db, err := sql.Open("sqlite", flagSqlitePath)
	if err != nil {
		fmt.Printf("%s\n", err)
		os.Exit(1)
	}
	// modernc.org/sqlite corrupts memory under concurrent writes against the
	// same file; serialize all access through a single connection.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	be := NewBackend(db)

	if flagIngest {
		ctx := context.Background()
		if err := Migrate(ctx, db, "pricing_migrations"); err != nil {
			fmt.Printf("%s\n", err)
			os.Exit(1)
		}
		if !lazySkipIngest(ctx, db, flagIngestRegion) {
			ingest(flagProvider, flagIngestRegion, be)
			_ = SetMeta(ctx, db, "last_ingest_at", time.Now().UTC().Format(time.RFC3339))
			_ = SetMeta(ctx, db, "last_ingest_region", flagIngestRegion)
		}
	}

	if flagestimatePlan != "" {
		var breakdownBuf bytes.Buffer
		breakdownOut := io.MultiWriter(os.Stdout, &breakdownBuf)

		plannedTotal, deltaTotal := estimatePlan(breakdownOut, flagestimatePlan, be)

		fmt.Printf("Planned monthly cost: $%s/mo\n", plannedTotal.StringFixed(2))
		fmt.Printf("Monthly cost increase: $%s/mo\n", deltaTotal.StringFixed(2))

		if flagPRComment {
			if err := postPRComment(breakdownBuf.String(), plannedTotal, deltaTotal); err != nil {
				fmt.Fprintf(os.Stderr, "failed to post PR comment: %s\n", err)
				os.Exit(1)
			}
		}
	}

}
func lazySkipIngest(ctx context.Context, db *sql.DB, region string) bool {
	if !flagLazy {
		return false
	}
	prevAt, _ := GetMeta(ctx, db, "last_ingest_at")
	prevRegion, _ := GetMeta(ctx, db, "last_ingest_region")
	if prevAt == "" || prevRegion != region {
		return false
	}
	t, err := time.Parse(time.RFC3339, prevAt)
	if err != nil {
		fmt.Printf("Re-ingesting: stored last_ingest_at %q is unparseable\n", prevAt)
		return false
	}
	age := time.Since(t).Truncate(time.Second)
	if flagLazyMaxAge > 0 {
		if age > flagLazyMaxAge {
			fmt.Printf("Re-ingesting: last ingest was %s ago (> -lazy-max-age=%s)\n", age, flagLazyMaxAge)
			return false
		}
		remaining := (flagLazyMaxAge - age).Truncate(time.Second)
		fmt.Printf("Skipping ingest: last ingest %s ago for region %s, expires in %s (drop -lazy to force)\n",
			age, prevRegion, remaining)
	} else {
		fmt.Printf("Skipping ingest: last ingest %s ago for region %s (drop -lazy to force)\n",
			age, prevRegion)
	}
	return true
}

func ingest(flagProvider string, region string, be backend.Backend) {
	fmt.Printf("Ingestion %s\n", flagProvider)

	if flagProvider != "aws" {
		fmt.Printf("unsupported provider %q (only aws is supported)\n", flagProvider)
		os.Exit(1)
	}

	for _, s := range aws.GetSupportedServices() {
		fmt.Printf("[%s] Ingestion\n", s)
		// Default buffer is 100 MiB per service, which spikes RSS unnecessarily
		// since we stream the CSV row-by-row. 1 MiB is plenty for bufio over the
		// HTTP body.
		op := []aws.Option{aws.WithBufferSize(1 << 20)}
		if flagIngestMinimal {
			op = append(op, aws.WithIngestionFilter(aws.MinimalFilter))
		}
		ingester, err := aws.NewIngester(s, region, op...)
		if err != nil {
			fmt.Printf("%s\n", err)
			os.Exit(1)
		}

		err = terracost.IngestPricing(context.Background(), be, ingester)
		if err != nil {
			fmt.Printf("%s\n", err)
			os.Exit(1)
		}
	}
}

func estimatePlan(out io.Writer, path string, be backend.Backend) (planned, delta decimal.Decimal) {
	fmt.Fprintf(out, "EstimateTerraformPlan\n")
	file, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(out, "%s\n", err)
		os.Exit(1)
	}

	plan, err := terracost.EstimateTerraformPlan(context.Background(), be, file, usage.Default)
	if err != nil {
		fmt.Fprintf(out, "%s\n", err)
		os.Exit(1)
	}

	return estimateDisplay(out, plan)
}

func estimateDisplay(out io.Writer, resourceDiff *cost.Plan) (planned, delta decimal.Decimal) {
	planned = decimal.Zero
	delta = decimal.Zero
	for _, res := range resourceDiff.ResourceDifferences() {
		priorCost, err := res.PriorCost()
		if err != nil {
			fmt.Fprintf(out, "PriorCost %s: %s\n", res.Address, err)
			continue
		}

		plannedCost, err := res.PlannedCost()
		if err != nil {
			fmt.Fprintf(out, "PlannedCost %s: %s\n", res.Address, err)
			continue
		}
		fmt.Fprintf(out, "%s: %s -> %s\n", res.Address, priorCost, plannedCost)
		planned = planned.Add(plannedCost.Monthly())
		delta = delta.Add(plannedCost.Monthly().Sub(priorCost.Monthly()))
	}
	return planned, delta
}

