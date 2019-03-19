package xormigrate

import (
	"errors"
	"fmt"

	"github.com/go-xorm/xorm"
)

const (
	initSchemaMigrationId = "SCHEMA_INIT"
)

// MigrateFunc is the func signature for migratinx.
type MigrateFunc func(*xorm.Session) error

// RollbackFunc is the func signature for rollbackinx.
type RollbackFunc func(*xorm.Session) error

// InitSchemaFunc is the func signature for initializing the schema.
type InitSchemaFunc func(*xorm.Session) error

// Migration represents a database migration (a modification to be made on the database).
type Migration struct {
	// ID is the migration identifier. Usually a timestamp like "201601021504".
	ID string `xorm:"id"`
	// Migrate is a function that will br executed while running this migration.
	Migrate MigrateFunc `xorm:"-"`
	// Rollback will be executed on rollback. Can be nil.
	Rollback RollbackFunc `xorm:"-"`
}

// Xormigrate represents a collection of all migrations of a database schema.
type Xormigrate struct {
	db         *xorm.Engine
	tx         *xorm.Session
	migrations []*Migration
	initSchema InitSchemaFunc
}

// ReservedIDError is returned when a migration is using a reserved ID
type ReservedIDError struct {
	ID string
}

func (e *ReservedIDError) Error() string {
	return fmt.Sprintf(`xormigrate: Reserved migration ID: "%s"`, e.ID)
}

// DuplicatedIDError is returned when more than one migration have the same ID
type DuplicatedIDError struct {
	ID string
}

func (e *DuplicatedIDError) Error() string {
	return fmt.Sprintf(`xormigrate: Duplicated migration ID: "%s"`, e.ID)
}

var (
	// ErrRollbackImpossible is returned when trying to rollback a migration
	// that has no rollback function.
	ErrRollbackImpossible = errors.New("xormigrate: It's impossible to rollback this migration")

	// ErrNoMigrationDefined is returned when no migration is defined.
	ErrNoMigrationDefined = errors.New("xormigrate: No migration defined")

	// ErrMissingID is returned when the ID od migration is equal to ""
	ErrMissingID = errors.New("xormigrate: Missing ID in migration")

	// ErrNoRunMigration is returned when any run migration was found while
	// running RollbackLast
	ErrNoRunMigration = errors.New("xormigrate: Could not find last run migration")

	// ErrMigrationIDDoesNotExist is returned when migrating or rolling back to a migration ID that
	// does not exist in the list of migrations
	ErrMigrationIDDoesNotExist = errors.New("xormigrate: Tried to migrate to an ID that doesn't exist")
)

// New returns a new Xormigrate.
func New(db *xorm.Engine, migrations []*Migration) *Xormigrate {
	return &Xormigrate{
		db:         db,
		migrations: migrations,
	}
}

// InitSchema sets a function that is run if no migration is found.
// The idea is preventing to run all migrations when a new clean database
// is being migratinx. In this function you should create all tables and
// foreign key necessary to your application.
func (x *Xormigrate) InitSchema(initSchema InitSchemaFunc) {
	x.initSchema = initSchema
}

// Migrate executes all migrations that did not run yet.
func (x *Xormigrate) Migrate() error {
	if !x.hasMigrations() {
		return ErrNoMigrationDefined
	}
	var targetMigrationID string
	if len(x.migrations) > 0 {
		targetMigrationID = x.migrations[len(x.migrations)-1].ID
	}
	return x.migrate(targetMigrationID)
}

// MigrateTo executes all migrations that did not run yet up to the migration that matches `migrationID`.
func (x *Xormigrate) MigrateTo(migrationID string) error {
	if err := x.checkIDExist(migrationID); err != nil {
		return err
	}
	return x.migrate(migrationID)
}

func (x *Xormigrate) migrate(migrationID string) error {
	if !x.hasMigrations() {
		return ErrNoMigrationDefined
	}

	if err := x.checkReservedID(); err != nil {
		return err
	}

	if err := x.checkDuplicatedID(); err != nil {
		return err
	}

	if err := x.createMigrationTableIfNotExists(); err != nil {
		return err
	}

	tx := x.db.NewSession()
	if err := tx.Begin(); err != nil {
		// if returned then will rollback automatically
		return err
	}
	defer tx.Close()

	if x.initSchema != nil && x.canInitializeSchema() {
		if err := x.runInitSchema(tx); err != nil {
			return err
		}
		return tx.Commit()
	}

	for _, migration := range x.migrations {
		if err := x.runMigration(tx, migration); err != nil {
			return err
		}
		if migrationID != "" && migration.ID == migrationID {
			break
		}
	}

	return tx.Commit()
}

// There are migrations to apply if either there's a defined
// initSchema function or if the list of migrations is not empty.
func (x *Xormigrate) hasMigrations() bool {
	return x.initSchema != nil || len(x.migrations) > 0
}

// Check whether any migration is using a reserved ID.
// For now there's only have one reserved ID, but there may be more in the future.
func (x *Xormigrate) checkReservedID() error {
	for _, m := range x.migrations {
		if m.ID == initSchemaMigrationId {
			return &ReservedIDError{ID: m.ID}
		}
	}
	return nil
}

func (x *Xormigrate) checkDuplicatedID() error {
	lookup := make(map[string]struct{}, len(x.migrations))
	for _, m := range x.migrations {
		if _, ok := lookup[m.ID]; ok {
			return &DuplicatedIDError{ID: m.ID}
		}
		lookup[m.ID] = struct{}{}
	}
	return nil
}

func (x *Xormigrate) checkIDExist(migrationID string) error {
	for _, migrate := range x.migrations {
		if migrate.ID == migrationID {
			return nil
		}
	}
	return ErrMigrationIDDoesNotExist
}

// RollbackLast undo the last migration
func (x *Xormigrate) RollbackLast() error {
	if len(x.migrations) == 0 {
		return ErrNoMigrationDefined
	}

	lastRunMigration, err := x.getLastRunMigration()
	if err != nil {
		return err
	}

	if err := x.RollbackMigration(lastRunMigration); err != nil {
		return err
	}
	return nil
}

// RollbackTo undoes migrations up to the given migration that matches the `migrationID`.
// Migration with the matching `migrationID` is not rolled back.
func (x *Xormigrate) RollbackTo(migrationID string) error {
	if len(x.migrations) == 0 {
		return ErrNoMigrationDefined
	}

	if err := x.checkIDExist(migrationID); err != nil {
		return err
	}

	tx := x.db.NewSession()
	if err := tx.Begin(); err != nil {
		// if returned then will rollback automatically
		return err
	}
	defer tx.Close()

	for i := len(x.migrations) - 1; i >= 0; i-- {
		migration := x.migrations[i]
		if migration.ID == migrationID {
			break
		}
		if x.migrationDidRun(migration) {
			if err := x.rollbackMigration(tx, migration); err != nil {
				return err
			}
		}
	}

	return tx.Commit()
}

func (x *Xormigrate) getLastRunMigration() (*Migration, error) {
	for i := len(x.migrations) - 1; i >= 0; i-- {
		migration := x.migrations[i]
		if x.migrationDidRun(migration) {
			return migration, nil
		}
	}
	return nil, ErrNoRunMigration
}

// RollbackMigration undo a migration.
func (x *Xormigrate) RollbackMigration(m *Migration) error {
	tx := x.db.NewSession()
	if err := tx.Begin(); err != nil {
		// if returned then will rollback automatically
		return err
	}
	defer tx.Close()
	if err := x.rollbackMigration(tx, m); err != nil {
		return err
	}
	return tx.Commit()
}

func (x *Xormigrate) rollbackMigration(tx *xorm.Session, m *Migration) error {
	if m.Rollback == nil {
		return ErrRollbackImpossible
	}

	if err := m.Rollback(tx); err != nil {
		return err
	}
	if _, err := x.db.In("id", m.ID).Delete(&Migration{}); err != nil {
		return err
	}
	return nil
}

func (x *Xormigrate) runInitSchema(tx *xorm.Session) error {
	if err := x.initSchema(tx); err != nil {
		return err
	}
	if err := x.insertMigration(initSchemaMigrationId); err != nil {
		return err
	}

	for _, migration := range x.migrations {
		if err := x.insertMigration(migration.ID); err != nil {
			return err
		}
	}

	return nil
}

func (x *Xormigrate) runMigration(tx *xorm.Session, migration *Migration) error {
	if len(migration.ID) == 0 {
		return ErrMissingID
	}

	if !x.migrationDidRun(migration) {
		if err := migration.Migrate(tx); err != nil {
			return err
		}

		if err := x.insertMigration(migration.ID); err != nil {
			return err
		}
	}
	return nil
}

func (x *Xormigrate) createMigrationTableIfNotExists() error {
	err := x.db.Sync2(new(Migration))
	return err
}

func (x *Xormigrate) migrationDidRun(m *Migration) bool {
	count, err := x.db.
		In("id", m.ID).
		Count(&Migration{})
	if err != nil {
		return false
	}
	return count > 0
}

// The schema can be initialised only if it hasn't been initialised yet
// and no other migration has been applied already.
func (x *Xormigrate) canInitializeSchema() bool {
	if x.migrationDidRun(&Migration{ID: initSchemaMigrationId}) {
		return false
	}

	// If the ID doesn't exist, we also want the list of migrations to be empty
	count, err := x.db.
		Count(&Migration{})
	if err != nil {
		return false
	}
	return count == 0
}

func (x *Xormigrate) insertMigration(id string) error {
	_, err := x.db.Insert(&Migration{ID: id})
	return err
}