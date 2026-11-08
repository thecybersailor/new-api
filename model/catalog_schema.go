package model

import (
	"fmt"
	"slices"
	"strings"

	"gorm.io/gorm"
)

// validateCatalogSchema is read-only. Existing catalog tables must already be
// usable; AutoMigrate must not silently adopt a hand-created residual schema.
// Only a master before migration may encounter absent tables.
func validateCatalogSchema(db *gorm.DB, allowMissing bool) error {
	for _, value := range CatalogIntegrationModels() {
		stmt := &gorm.Statement{DB: db}
		if err := stmt.Parse(value); err != nil {
			return fmt.Errorf("catalog schema: %w", err)
		}
		table := stmt.Schema.Table
		exists, valid, err := catalogPrimaryKeyState(db, table)
		if err != nil {
			return fmt.Errorf("catalog schema %s: inspect primary key: %w", table, err)
		}
		if !exists && allowMissing {
			continue
		}
		if !exists {
			return fmt.Errorf("catalog schema %s is missing; complete master migration before starting this node", table)
		}
		if !valid {
			return fmt.Errorf("catalog schema %s requires a generated single-column integer id primary key; review the existing schema before restart", table)
		}
		// A projection checks all mapped columns and SELECT permissions without
		// reading rows or attempting to add missing fields to a residual table.
		rows, err := db.Table(table).Select(stmt.Schema.DBNames).Limit(0).Rows()
		if err != nil {
			return fmt.Errorf("catalog schema %s: inspect required columns: %w", table, err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("catalog schema %s: close inspection: %w", table, err)
		}
		indexes := stmt.Schema.ParseIndexes()
		names := make([]string, 0, len(indexes))
		for name := range indexes {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			expected := indexes[name]
			columns := make([]string, len(expected.Fields))
			for i, field := range expected.Fields {
				columns[i] = field.DBName
			}
			valid, err := catalogIndexMatches(db, table, name, columns, expected.Class == "UNIQUE")
			if err != nil {
				return fmt.Errorf("catalog schema %s: inspect index %s: %w", table, name, err)
			}
			if !valid {
				return fmt.Errorf("catalog schema %s has missing or incompatible index %s; review the existing schema before restart", table, name)
			}
		}
	}
	return nil
}

func catalogPrimaryKeyState(db *gorm.DB, table string) (exists, valid bool, err error) {
	switch db.Dialector.Name() {
	case "sqlite":
		var object struct{ Type string }
		result := db.Raw("SELECT type FROM sqlite_master WHERE name = ?", table).Scan(&object)
		if result.Error != nil || result.RowsAffected == 0 {
			return false, false, result.Error
		}
		if object.Type != "table" {
			return true, false, nil
		}
		var columns []struct {
			Name string
			Type string
			PK   int
		}
		if err := db.Raw("SELECT name,type,pk FROM pragma_table_info(?)", table).Scan(&columns).Error; err != nil {
			return true, false, err
		}
		primaryCount := 0
		for _, column := range columns {
			if column.PK != 0 {
				primaryCount++
				valid = column.Name == "id" && strings.EqualFold(column.Type, "INTEGER")
			}
		}
		// WITHOUT ROWID and INTEGER PRIMARY KEY DESC both have a separate PK
		// index; neither aliases rowid nor generates the id expected by GORM.
		var separatePK int64
		if err := db.Raw("SELECT count(*) FROM pragma_index_list(?) WHERE origin = 'pk'", table).Scan(&separatePK).Error; err != nil {
			return true, false, err
		}
		return true, valid && primaryCount == 1 && separatePK == 0, nil
	case "mysql":
		var state struct{ Valid bool }
		result := db.Raw(`SELECT (t.TABLE_TYPE = 'BASE TABLE' AND
 (SELECT count(*) FROM information_schema.COLUMNS c WHERE c.TABLE_SCHEMA=t.TABLE_SCHEMA AND c.TABLE_NAME=t.TABLE_NAME AND c.COLUMN_KEY='PRI') = 1 AND
 EXISTS (SELECT 1 FROM information_schema.COLUMNS c WHERE c.TABLE_SCHEMA=t.TABLE_SCHEMA AND c.TABLE_NAME=t.TABLE_NAME AND c.COLUMN_NAME='id' AND c.COLUMN_KEY='PRI' AND c.DATA_TYPE='bigint' AND c.EXTRA LIKE '%auto_increment%')) AS valid
 FROM information_schema.TABLES t WHERE t.TABLE_SCHEMA=DATABASE() AND t.TABLE_NAME=?`, table).Scan(&state)
		return result.RowsAffected == 1, state.Valid, result.Error
	case "postgres":
		var state struct{ Valid bool }
		// to_jsonb safely exposes attidentity only on versions that have it.
		// pg_get_serial_sequence, pg_attrdef and this JSON operation exist in 9.6.
		// Checking metadata never calls nextval or writes a probe row.
		result := db.Raw(`SELECT (c.relkind='r' AND EXISTS (
 SELECT 1 FROM pg_attribute a JOIN pg_index i ON i.indrelid=c.oid AND i.indisprimary
 LEFT JOIN pg_attrdef d ON d.adrelid=c.oid AND d.adnum=a.attnum
 WHERE a.attrelid=c.oid AND a.attname='id' AND NOT a.attisdropped AND a.atttypid='int8'::regtype
 AND i.indnatts=1 AND i.indkey[0]=a.attnum AND i.indisvalid
 AND pg_get_serial_sequence(c.oid::regclass::text,'id') IS NOT NULL
 AND (COALESCE(to_jsonb(a)->>'attidentity','') IN ('a','d') OR
 (pg_get_expr(d.adbin,d.adrelid) = 'nextval(' || quote_literal(pg_get_serial_sequence(c.oid::regclass::text,'id')::regclass::text) || '::regclass)'
 AND (has_sequence_privilege(pg_get_serial_sequence(c.oid::regclass::text,'id'),'USAGE') OR has_sequence_privilege(pg_get_serial_sequence(c.oid::regclass::text,'id'),'UPDATE'))))
 )) AS valid FROM pg_class c WHERE c.oid=to_regclass(?)`, table).Scan(&state)
		return result.RowsAffected == 1, state.Valid, result.Error
	default:
		return false, false, fmt.Errorf("unsupported database %q", db.Dialector.Name())
	}
}

func catalogIndexMatches(db *gorm.DB, table, name string, expected []string, unique bool) (bool, error) {
	var columns []struct{ Name string }
	switch db.Dialector.Name() {
	case "sqlite":
		var indexes []struct {
			Name    string
			Unique  bool
			Partial bool
		}
		if err := db.Raw("SELECT name,\"unique\",partial FROM pragma_index_list(?)", table).Scan(&indexes).Error; err != nil {
			return false, err
		}
		found := false
		for _, index := range indexes {
			if index.Name == name {
				found = index.Unique == unique && !index.Partial
			}
		}
		if !found {
			return false, nil
		}
		if err := db.Raw("SELECT name FROM pragma_index_info(?) ORDER BY seqno", name).Scan(&columns).Error; err != nil {
			return false, err
		}
	case "mysql":
		var indexes []struct {
			Name      string
			NonUnique bool
			SubPart   *int
		}
		if err := db.Raw("SELECT COLUMN_NAME AS name, NON_UNIQUE AS non_unique, SUB_PART AS sub_part FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND INDEX_NAME=? ORDER BY SEQ_IN_INDEX", table, name).Scan(&indexes).Error; err != nil {
			return false, err
		}
		for _, index := range indexes {
			if index.NonUnique == unique || index.SubPart != nil {
				return false, nil
			}
			columns = append(columns, struct{ Name string }{index.Name})
		}
	case "postgres":
		if err := db.Raw(`SELECT a.attname AS name FROM pg_index i
 JOIN pg_class c ON c.oid=i.indexrelid
 CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum,position)
 JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum
 WHERE i.indrelid=to_regclass(?) AND c.relname=? AND i.indisunique=?
 AND i.indisvalid AND i.indisready AND i.indpred IS NULL AND i.indexprs IS NULL ORDER BY k.position`, table, name, unique).Scan(&columns).Error; err != nil {
			return false, err
		}
	default:
		return false, fmt.Errorf("unsupported database %q", db.Dialector.Name())
	}
	actual := make([]string, len(columns))
	for i, column := range columns {
		actual[i] = column.Name
	}
	return slices.Equal(actual, expected), nil
}
