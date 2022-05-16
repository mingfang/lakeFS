package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v4"

	"github.com/treeverse/lakefs/pkg/kv"

	"github.com/hashicorp/go-multierror"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/treeverse/lakefs/pkg/db/params"
	"github.com/treeverse/lakefs/pkg/logging"
)

// TODO: (niro) Remove module on feature complete

type MigrateFunc func(ctx context.Context, db *pgxpool.Pool, writer io.Writer) error

var (
	kvPkgs     = make(map[string]pkgMigrate)
	registerMu sync.RWMutex
)

type pkgMigrate struct {
	Func   MigrateFunc
	Tables []string
}

// Migrate data migration from DB to KV
func Migrate(ctx context.Context, dbPool *pgxpool.Pool, dbParams params.Database) error {
	if !dbParams.KVEnabled {
		return nil
	}

	store, err := kv.Open(ctx, DriverName, dbParams.ConnectionString)
	if err != nil {
		return fmt.Errorf("opening kv store: %w", err)
	}
	defer store.Close()

	shouldDrop, err := validateVersion(ctx, store)
	if err != nil {
		return fmt.Errorf("validating version: %w", err)
	}
	if shouldDrop {
		// After unsuccessful migration attempt, clean KV table
		// Delete store if exists from previous failed KV migration and reopen store
		logging.Default().Warn("Removing KV table")
		err = dropTables(ctx, dbPool, []string{DefaultTableName})
		if err != nil {
			return err
		}
		tmpStore, err := kv.Open(ctx, DriverName, dbParams.ConnectionString) // Open flow recreates table
		if err != nil {
			return fmt.Errorf("opening kv store: %w", err)
		}
		tmpStore.Close()
	}

	// Mark KV Migration started
	err = store.Set(ctx, []byte(kv.DBVersionPath), []byte("0"))
	if err != nil {
		return err
	}

	// Import to KV Store
	var g multierror.Group
	var tables []string
	tmpDir, err := os.MkdirTemp("", "kv_migrate_")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	logger := logging.Default().WithField("TempDir", tmpDir)
	logger.Info("Starting KV Migration Process")
	for n, p := range kvPkgs {
		name := n
		migrateFunc := p.Func
		tables = append(tables, p.Tables...)
		g.Go(func() error {
			fileLog := logging.Default().WithField("pkg_id", name)
			fileLog.Info("Starting KV migration for package")
			fd, err := os.CreateTemp(tmpDir, fmt.Sprintf("migrate_%s_", name))
			if err != nil {
				return fmt.Errorf("create temp file: %w", err)
			}
			defer fd.Close()
			err = migrateFunc(ctx, dbPool, fd)
			if err != nil {
				fileLog.WithError(err).Error()
				return fmt.Errorf("failed migration on package %s: %w", name, err)
			}
			_, err = fd.Seek(0, 0)
			if err != nil {
				return fmt.Errorf("failed seek file on package %s: %w", name, err)
			}
			err = kv.Import(ctx, fd, store)
			if err != nil {
				return fmt.Errorf("failed import on package %s: %w", name, err)
			}
			fileLog.Info("Successfully migrated package to KV")
			return nil
		})
	}
	err = g.Wait().ErrorOrNil()
	if err != nil {
		return err
	}

	// Update migrate version
	err = store.Set(ctx, []byte(kv.DBVersionPath), []byte(strconv.Itoa(kv.InitialMigrateVersion)))
	if err != nil {
		return fmt.Errorf("failed setting migrate version: %w", err)
	}

	if dbParams.DropTables {
		err = dropTables(ctx, dbPool, tables)
		if err != nil {
			return err
		}
	}
	if err = os.RemoveAll(tmpDir); err != nil {
		logger.Error("Failed to remove migration directory") // This should not fail the migration process
	}
	return nil
}

// validateVersion Check KV version before migration. Version exists and smaller than InitialMigrateVersion indicates
// failed previous migration. In this case we drop the kv table and recreate it.
// In case version is equal or bigger - it means we already performed a successful migration and there's nothing to do
func validateVersion(ctx context.Context, store kv.Store) (bool, error) {
	version, err := kv.GetDBVersion(ctx, store)
	if err != nil && errors.Is(err, kv.ErrNotFound) {
		return false, nil
	} else if err == nil { // Version exists in DB
		return version < kv.InitialMigrateVersion, nil
	}
	return false, err
}

func Register(name string, f MigrateFunc, tables []string) {
	registerMu.Lock()
	defer registerMu.Unlock()
	if _, ok := kvPkgs[name]; ok {
		panic(fmt.Sprintf("Package already registered: %s", name))
	}
	kvPkgs[name] = pkgMigrate{
		Func:   f,
		Tables: tables,
	}
}

// UnRegisterAll remove all loaded migrate callbacks, used for test code.
func UnRegisterAll() {
	registerMu.Lock()
	defer registerMu.Unlock()
	for k := range kvPkgs {
		delete(kvPkgs, k)
	}
}

func dropTables(ctx context.Context, dbPool *pgxpool.Pool, tables []string) error {
	if len(tables) < 1 {
		return nil
	}
	for i, table := range tables {
		tables[i] = pgx.Identifier{table}.Sanitize()
	}
	_, err := dbPool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, strings.Join(tables, ", ")))
	if err != nil {
		return fmt.Errorf("failed during drop tables: %s. Please perform manual cleanup of old database tables: %w", tables, err)
	}
	return nil
}
