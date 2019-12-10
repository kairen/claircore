package postgres

import (
	"context"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"io"
	"time"

	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/rs/zerolog"

	"github.com/quay/claircore"
	"github.com/quay/claircore/internal/vulnstore"
	"github.com/quay/claircore/libvuln/driver"
)

func get(ctx context.Context, pool *pgxpool.Pool, records []*claircore.IndexRecord, opts vulnstore.GetOpts) (map[int][]*claircore.Vulnerability, error) {
	log := zerolog.Ctx(ctx).With().
		Str("component", "vulnstore.get").
		Logger()
	// Build our query we will make into a prepared statement. See build func
	// definition for details and context.
	query, dedupedMatchers, err := getBuilder(opts.Matchers)
	if err != nil {
		return nil, err
	}
	h := fnv.New64a()
	if _, err := io.WriteString(h, query); err != nil {
		return nil, err
	}
	name := hex.EncodeToString(h.Sum(nil))
	log.Debug().Str("name", name).Msg("built query")

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Create a prepared statement.
	getStmt, err := tx.Prepare(ctx, name, query)
	if err != nil {
		return nil, err
	}

	// start a batch
	batch := &pgx.Batch{}

	// create our bind arguments. the order of dedupedMatchers
	// dictates the order of our bindvar values.
	for _, record := range records {
		args := []interface{}{}
		// fills the OR bind vars for (package_name = binary_package OR package_name = source_package)
		args = append(args, record.Package.Source.Name)
		args = append(args, record.Package.Name)

		for _, m := range dedupedMatchers {
			switch m {
			case driver.DistributionDID:
				args = append(args, record.Distribution.DID)
			case driver.DistributionName:
				args = append(args, record.Distribution.Name)
			case driver.DistributionVersion:
				args = append(args, record.Distribution.Version)
			case driver.DistributionVersionCodeName:
				args = append(args, record.Distribution.VersionCodeName)
			case driver.DistributionVersionID:
				args = append(args, record.Distribution.VersionID)
			case driver.DistributionArch:
				args = append(args, record.Distribution.Arch)
			case driver.DistributionCPE:
				args = append(args, record.Distribution.CPE)
			case driver.DistributionPrettyName:
				args = append(args, record.Distribution.PrettyName)
			}
		}
		// queue the select query
		batch.Queue(getStmt.Name, args...)
	}
	// send the batch
	tctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res := tx.SendBatch(tctx, batch)
	// Can't just defer the close, because the batch must be fully handled
	// before resolving the transaction. Maybe we can move this result handling
	// into its own function to be able to just defer it.

	// gather all the returned vulns for each queued select statement
	results := make(map[int][]*claircore.Vulnerability)
	for _, record := range records {
		rows, err := res.Query()
		if err != nil {
			res.Close()
			return nil, err
		}

		// unpack all returned rows into claircore.Vulnerability structs
		for rows.Next() {
			// fully allocate vuln struct
			v := &claircore.Vulnerability{
				Package: &claircore.Package{},
				Dist:    &claircore.Distribution{},
				Repo:    &claircore.Repository{},
			}

			err := rows.Scan(
				&v.ID,
				&v.Name,
				&v.Description,
				&v.Links,
				&v.Severity,
				&v.Package.Name,
				&v.Package.Version,
				&v.Package.Kind,
				&v.Dist.DID,
				&v.Dist.Name,
				&v.Dist.Version,
				&v.Dist.VersionCodeName,
				&v.Dist.VersionID,
				&v.Dist.Arch,
				&v.Dist.CPE,
				&v.Repo.Name,
				&v.Repo.Key,
				&v.Repo.URI,
				&v.Dist.PrettyName,
				&v.FixedInVersion,
			)
			if err != nil {
				res.Close()
				return nil, fmt.Errorf("failed to scan vulnerability: %v", err)
			}

			// add vulernability to result. handle if array does not exist
			if _, ok := results[record.Package.ID]; !ok {
				vvulns := []*claircore.Vulnerability{v}
				results[record.Package.ID] = vvulns
			} else {
				results[record.Package.ID] = append(results[record.Package.ID], v)
			}
		}
	}
	if err := res.Close(); err != nil {
		return nil, fmt.Errorf("some weird batch error: %v", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("failed to commit tx: %v", err)
	}
	return results, nil
}
