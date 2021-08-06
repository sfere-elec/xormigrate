package xormigrate

import (
	"errors"
	"fmt"

	"xorm.io/xorm"
)

const (
	initSchemaMigrationID = "SCHEMA_INIT"
)

// MigrateFunc is the func signature for migratinx.
type MigrateFunc func(*xorm.Session) error

// RollbackFunc is the func signature for rollbackinx.
type RollbackFunc func(*xorm.Session) error

// InitSchemaFunc is the func signature for initializing the schema.
type InitSchemaFunc func(*xorm.Session) error

// Options define options for all migrations.
type Options struct {
	// TableName is the migration table.
	TableName string
	// IDColumnName is the name of column where the migration id will be stored.
	IDColumnName string
	// IDColumnSize is the length of the migration id column
	IDColumnSize int
	// UseTransaction makes Gormigrate execute migrations inside a single transaction.
	// Keep in mind that not all databases support DDL commands inside transactions.
	UseTransaction bool
	// ValidateUnknownMigrations will cause migrate to fail if there's unknown migration
	// IDs in the database
	ValidateUnknownMigrations bool
}

// Migration represents a database migration (a modification to be made on the database).
type Migration struct {
	// ID is the migration identifier. Usually a timestamp like "201601021504".
	ID string `xorm:"id"`
	// Description is the migration description, which is optionally printed out when the migration is ran.
	Description string `xorm:"description"`
	// Migrate is a function that will br executed while running this migration.
	Migrate MigrateFunc `xorm:"-"`
	// Rollback will be executed on rollback. Can be nil.
	Rollback RollbackFunc `xorm:"-"`
}

// Xormigrate represents a collection of all migrations of a database schema.
type Xormigrate struct {
	session    *xorm.Session
	options    *Options
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
	// DefaultOptions can be used if you don't want to think about options.
	DefaultOptions = &Options{
		TableName:                 "migrations",
		IDColumnName:              "id",
		IDColumnSize:              255,
		UseTransaction:            false,
		ValidateUnknownMigrations: false,
	}

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

	// ErrUnknownPastMigration is returned if a migration exists in the DB that doesn't exist in the code
	ErrUnknownPastMigration = errors.New("xormigrate: Found migration in DB that does not exist in code")
)

// New returns a new Xormigrate.
func New(session *xorm.Session, options *Options, migrations []*Migration) *Xormigrate {
	if options.TableName == "" {
		options.TableName = DefaultOptions.TableName
	}
	if options.IDColumnName == "" {
		options.IDColumnName = DefaultOptions.IDColumnName
	}
	if options.IDColumnSize == 0 {
		options.IDColumnSize = DefaultOptions.IDColumnSize
	}
	return &Xormigrate{
		session:    session,
		options:    options,
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

	x.begin()
	defer x.rollback()

	if err := x.createMigrationTableIfNotExists(); err != nil {
		return err
	}
	if x.options.ValidateUnknownMigrations {
		unknownMigrations, err := x.unknownMigrationsHaveHappened()
		if err != nil {
			return err
		}
		if unknownMigrations {
			return ErrUnknownPastMigration
		}
	}
	if x.initSchema != nil {
		canInitializeSchema, err := x.canInitializeSchema()
		if err != nil {
			return err
		}
		if canInitializeSchema {
			if err := x.runInitSchema(); err != nil {
				return err
			}
			return x.commit()
		}
	}
	for _, migration := range x.migrations {
		if err := x.runMigration(migration); err != nil {
			return err
		}
		if migrationID != "" && migration.ID == migrationID {
			break
		}
	}
	return x.commit()
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
		if m.ID == initSchemaMigrationID {
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

	x.begin()
	defer x.rollback()

	lastRunMigration, err := x.getLastRunMigration()
	if err != nil {
		return err
	}
	if err := x.rollbackMigration(lastRunMigration); err != nil {
		return err
	}
	return x.commit()
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

	x.begin()
	defer x.rollback()

	for i := len(x.migrations) - 1; i >= 0; i-- {
		migration := x.migrations[i]
		if migration.ID == migrationID {
			break
		}
		migrationRan, err := x.migrationRan(migration)
		if err != nil {
			return err
		}
		if migrationRan {
			if err := x.rollbackMigration(migration); err != nil {
				return err
			}
		}
	}
	return x.commit()
}

func (x *Xormigrate) getLastRunMigration() (*Migration, error) {
	for i := len(x.migrations) - 1; i >= 0; i-- {
		migration := x.migrations[i]
		migrationRan, err := x.migrationRan(migration)
		if err != nil {
			return nil, err
		}
		if migrationRan {
			return migration, nil
		}
	}
	return nil, ErrNoRunMigration
}

// RollbackMigration undo a migration.
func (x *Xormigrate) RollbackMigration(m *Migration) error {
	x.begin()
	defer x.rollback()

	if err := x.rollbackMigration(m); err != nil {
		return err
	}
	return x.commit()
}

func (x *Xormigrate) rollbackMigration(m *Migration) error {
	if m.Rollback == nil {
		return ErrRollbackImpossible
	}
	if err := m.Rollback(x.session); err != nil {
		return err
	}
	if _, err := x.session.Table(x.options.TableName).In("id", m.ID).Delete(&Migration{}); err != nil {
		return err
	}
	return nil
}

func (x *Xormigrate) runInitSchema() error {
	if err := x.initSchema(x.session); err != nil {
		return err
	}
	if err := x.insertMigration(initSchemaMigrationID); err != nil {
		return err
	}
	for _, migration := range x.migrations {
		if err := x.insertMigration(migration.ID); err != nil {
			return err
		}
	}
	return nil
}

func (x *Xormigrate) runMigration(migration *Migration) error {
	if len(migration.ID) == 0 {
		return ErrMissingID
	}
	migrationRan, err := x.migrationRan(migration)
	if err != nil {
		return err
	}
	if !migrationRan {
		if err := migration.Migrate(x.session); err != nil {
			return err
		}

		if err := x.insertMigration(migration.ID); err != nil {
			return err
		}
	}
	return nil
}

func (x *Xormigrate) createMigrationTableIfNotExists() error {
	b, err := x.session.IsTableExist(x.options.TableName)
	if b {
		return nil
	}
	if err != nil {
		return err
	}
	return x.session.Table(x.options.TableName).Sync2(new(Migration))
}

func (x *Xormigrate) migrationRan(m *Migration) (bool, error) {
	count, err := x.session.
		Table(x.options.TableName).
		In("id", m.ID).
		Count(&Migration{})
	return count > 0, err
}

// The schema can be initialized only if it hasn't been initialized yet
// and no other migration has been applied already.
func (x *Xormigrate) canInitializeSchema() (bool, error) {
	migrationRan, err := x.migrationRan(&Migration{ID: initSchemaMigrationID})
	if err != nil {
		return false, err
	}
	if migrationRan {
		return false, nil
	}

	// If the ID doesn't exist, we also want the list of migrations to be empty
	count, err := x.session.
		Table(x.options.TableName).
		Count(&Migration{})
	return count == 0, err
}

func (x *Xormigrate) unknownMigrationsHaveHappened() (bool, error) {
	rows, err := x.session.Table(x.options.TableName).Select(x.options.IDColumnName).Rows(&Migration{})
	if err != nil {
		return false, err
	}
	defer rows.Close()

	validIDSet := make(map[string]struct{}, len(x.migrations)+1)
	validIDSet[initSchemaMigrationID] = struct{}{}
	for _, migration := range x.migrations {
		validIDSet[migration.ID] = struct{}{}
	}

	for rows.Next() {
		var pastMigrationID string
		if err := rows.Scan(&pastMigrationID); err != nil {
			return false, err
		}
		if _, ok := validIDSet[pastMigrationID]; !ok {
			return true, nil
		}
	}

	return false, nil
}

func (x *Xormigrate) insertMigration(id string) error {
	_, err := x.session.Table(x.options.TableName).Insert(&Migration{ID: id})
	return err
}

func (x *Xormigrate) begin() {
	if x.options.UseTransaction {
		x.session.Begin()
	}
}

func (x *Xormigrate) commit() error {
	if x.options.UseTransaction {
		return x.session.Commit()
	}
	return nil
}

func (x *Xormigrate) rollback() {
	if x.options.UseTransaction {
		x.session.Rollback()
	}
}
