package main

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
)

// lakeName is the name the lake is attached under in every script. Model SQL
// never uses it: every script starts with USE lake.
const lakeName = "lake"

// lakeOptions say how to reach the lake: the flags shared with the other tools.
type lakeOptions struct {
	duckdb   string // duckdb CLI, 1.5 or newer
	endpoint string // S3 endpoint URL
	region   string
	profile  string // from ~/.aws
	stateDir string // where DuckDB keeps extensions and spill files
}

// lake runs scripts against the project's DuckLake.
type lake struct {
	*duck
	p    *project
	hist string // run_history table, schema.table
}

// openLake checks the DuckDB CLI, installs the extensions it needs, and
// creates the run history table of p's target if it is missing.
func openLake(ctx context.Context, p *project, o lakeOptions) (*lake, error) {
	if p.Lake.Catalog == "" {
		return nil, fmt.Errorf("project.yaml: lake.catalog is required, e.g. ducklake:postgres:dbname=ducklake")
	}
	bin, err := exec.LookPath(o.duckdb)
	if err != nil {
		return nil, fmt.Errorf("duckdb CLI %q not found (install DuckDB 1.5+ or pass -duckdb /path/to/duckdb): %w", o.duckdb, err)
	}

	// Model SQL sees the project's time zone, the one schedules use too.
	setup := []string{"SET TimeZone = " + ql(p.Timezone)}
	if o.stateDir != "" {
		setup = append(setup,
			"SET extension_directory = "+ql(filepath.Join(o.stateDir, "extensions")),
			"SET temp_directory = "+ql(filepath.Join(o.stateDir, "tmp")),
		)
	}
	install := []string{"INSTALL ducklake"}
	switch {
	case strings.HasPrefix(p.Lake.Catalog, "ducklake:postgres:"):
		install = append(install, "INSTALL postgres")
	case strings.HasPrefix(p.Lake.Catalog, "ducklake:sqlite:"):
		install = append(install, "INSTALL sqlite")
	}
	secret, err := s3Secret(ctx, o)
	if err != nil {
		return nil, err
	}
	if secret != "" {
		install = append(install, "INSTALL httpfs")
	}

	plain := &duck{bin: bin, setup: setup}
	res, err := plain.query(ctx, append(install, "SELECT version() AS v")...)
	if err != nil {
		return nil, fmt.Errorf("installing DuckDB extensions: %w", err)
	}
	var major, minor int
	v := res[0].str("v")
	fmt.Sscanf(v, "v%d.%d", &major, &minor)
	if major < 1 || major == 1 && minor < 5 {
		return nil, fmt.Errorf("%s is DuckDB %s; lakeshape needs 1.5 or newer", bin, v)
	}

	setup = append(setup, "LOAD ducklake")
	if secret != "" {
		// The script goes to duckdb on stdin, so the secret never shows up in
		// a process list, and it is temporary: nothing is saved.
		setup = append(setup, "LOAD httpfs", secret)
	}
	attach := "ATTACH " + ql(p.Lake.Catalog) + " AS " + lakeName
	if p.Lake.DataPath != "" {
		attach += " (DATA_PATH " + ql(p.Lake.DataPath) + ")"
	}
	setup = append(setup, attach, "USE "+lakeName)

	l := &lake{duck: &duck{bin: bin, setup: setup}, p: p, hist: p.target.schema(historySchema) + ".run_history"}
	schema, _, _ := strings.Cut(l.hist, ".")
	if err := p.target.checkWrite(schema); err != nil {
		return nil, err
	}
	var s script
	s.add("CREATE SCHEMA IF NOT EXISTS "+schema, `CREATE TABLE IF NOT EXISTS `+l.hist+` (
		run_id VARCHAR, jobs VARCHAR[], target VARCHAR, kind VARCHAR, name VARCHAR,
		relation VARCHAR, materialized VARCHAR, status VARCHAR, rows BIGINT, error VARCHAR,
		started_at TIMESTAMPTZ, finished_at TIMESTAMPTZ, snapshot_id BIGINT)`)
	if _, err := l.run(ctx, &s); err != nil {
		return nil, fmt.Errorf("opening the lake: %w", err)
	}
	return l, nil
}

// s3Secret returns a CREATE SECRET statement with the S3 credentials from the
// usual AWS environment variables or ~/.aws profile, or "" if there are none.
func s3Secret(ctx context.Context, o lakeOptions) (string, error) {
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithEC2IMDSClientEnableState(imds.ClientDisabled)}
	if o.profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(o.profile))
	}
	if o.region != "" {
		opts = append(opts, awsconfig.WithRegion(o.region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return "", fmt.Errorf("loading AWS config: %w", err)
	}
	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil || creds.AccessKeyID == "" {
		return "", nil // no S3: fine for a lake on local disk
	}
	region := cfg.Region
	if region == "" {
		region = "garage"
	}
	secret := []string{"TYPE s3", "REGION " + ql(region), "URL_STYLE 'path'",
		"KEY_ID " + ql(creds.AccessKeyID), "SECRET " + ql(creds.SecretAccessKey)}
	if creds.SessionToken != "" {
		secret = append(secret, "SESSION_TOKEN "+ql(creds.SessionToken))
	}
	if o.endpoint != "" {
		u, err := url.Parse(o.endpoint)
		if err != nil || u.Host == "" {
			return "", fmt.Errorf("endpoint %q: want a URL like http://host:3900", o.endpoint)
		}
		secret = append(secret, "ENDPOINT "+ql(u.Host), fmt.Sprintf("USE_SSL %t", u.Scheme == "https"))
	}
	return "CREATE TEMPORARY SECRET lakeshape_s3 (" + strings.Join(secret, ", ") + ")", nil
}

// relations returns the tables and views in the lake, as schema.name -> "table" or "view".
func (l *lake) relations(ctx context.Context) (map[string]string, error) {
	rows, err := l.query(ctx, `SELECT schema_name AS s, table_name AS n, 'table' AS k FROM duckdb_tables() WHERE database_name = '`+lakeName+`'
		UNION ALL SELECT schema_name, view_name, 'view' FROM duckdb_views() WHERE database_name = '`+lakeName+`' AND NOT internal`)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, r := range rows {
		out[r.str("s")+"."+r.str("n")] = r.str("k")
	}
	return out, nil
}
