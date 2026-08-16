package migrator

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// DryRunReport describes the full schema translation plan for a migration
// without modifying the target database or session storage.
type DryRunReport struct {
	SourceDatabase  string        `json:"source_database"`
	TargetSchema    string        `json:"target_schema"`
	TotalTables     int           `json:"total_tables"`
	SelectedTables  int           `json:"selected_tables"`
	Tables          []TableDryRun `json:"tables"`
	Views           []string      `json:"views"`
	Functions       []string      `json:"functions"`
	Issues          []string      `json:"issues"`
	ManualWorkItems int           `json:"manual_work_items"`
	IssueCount      int           `json:"issue_count"`
	LoadMethod      LoadMethod    `json:"load_method"`
}

// TableDryRun describes the plan for one table.
type TableDryRun struct {
	Name          string           `json:"name"`
	EstimatedRows int64            `json:"estimated_rows"`
	PrimaryKey    []string         `json:"primary_key"`
	Resumable     bool             `json:"resumable"`
	Columns       []ColumnDryRun   `json:"columns"`
	EnumTypes     []EnumTypeDryRun `json:"enum_types"`
	Indexes       []IndexDryRun    `json:"indexes"`
	ForeignKeys   []ForeignKeyPlan `json:"foreign_keys"`
	Triggers      []string         `json:"triggers"`
	CreateSQL     string           `json:"create_sql,omitempty"`
	Issues        []string         `json:"issues"`
	Warnings      []string         `json:"warnings"`
}

// ColumnDryRun describes how one column will be translated.
type ColumnDryRun struct {
	Name         string `json:"name"`
	MySQLType    string `json:"mysql_type"`
	PostgresType string `json:"postgres_type"`
	Nullable     bool   `json:"nullable"`
	Issue        string `json:"issue,omitempty"`
}

// EnumTypeDryRun describes a PostgreSQL enum type that would be created.
type EnumTypeDryRun struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

// IndexDryRun describes a secondary index that would be created.
type IndexDryRun struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique"`
	Issue   string   `json:"issue,omitempty"`
}

// ForeignKeyPlan describes a foreign key that would be created.
type ForeignKeyPlan struct {
	Name              string   `json:"name"`
	Columns           []string `json:"columns"`
	ReferencedTable   string   `json:"referenced_table"`
	ReferencedColumns []string `json:"referenced_columns"`
	OnDelete          string   `json:"on_delete,omitempty"`
	OnUpdate          string   `json:"on_update,omitempty"`
	// SourceMissing marks a constraint whose referenced table does not exist
	// in the source database (partial backup); it is skipped and reported
	// for manual work instead of being created. SQL then carries the exact
	// DDL to run once the referenced table is available.
	SourceMissing bool   `json:"source_missing,omitempty"`
	SQL           string `json:"sql,omitempty"`
	// OrphanRows counts child rows violating the constraint in the source;
	// the constraint is skipped and saved for manual work until repaired.
	OrphanRows int64 `json:"orphan_rows,omitempty"`
}

// DryRun computes the full migration plan by reading MySQL metadata only.
// It performs no writes against PostgreSQL or the session store, and unlike
// Start it collects every issue it finds instead of stopping at the first.
func (m *Migrator) DryRun(ctx context.Context) (*DryRunReport, error) {
	report := &DryRunReport{
		TargetSchema: m.schemaName,
		LoadMethod:   m.loadMethod,
	}

	if err := m.sourceQueryer(ctx).QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&report.SourceDatabase); err != nil {
		return nil, fmt.Errorf("read source database name: %w", err)
	}

	sourceTables, err := m.getMySQLTables(ctx)
	if err != nil {
		return nil, err
	}
	report.TotalTables = len(sourceTables)

	names := make([]string, len(sourceTables))
	enginesByName := make(map[string]string, len(sourceTables))
	for i, table := range sourceTables {
		names[i] = table.name
		enginesByName[strings.ToLower(table.name)] = table.engine
	}
	tables, err := m.filter.apply(names)
	if err != nil {
		return nil, err
	}
	report.SelectedTables = len(tables)

	if err := m.resolveUUIDConversions(ctx, tables); err != nil {
		report.Issues = append(report.Issues, fmt.Sprintf("resolve UUID conversions: %v", err))
	}

	if m.filter.active() {
		if err := m.validateForeignKeyClosure(ctx, tables); err != nil {
			report.Issues = append(report.Issues, err.Error())
		}
	}
	if err := m.validateSourceForeignKeyScope(ctx); err != nil {
		report.Issues = append(report.Issues, err.Error())
	}

	for _, table := range tables {
		tableReport := m.dryRunTable(ctx, table, enginesByName[strings.ToLower(table)], enginesByName)
		report.Tables = append(report.Tables, tableReport)
	}

	views, err := m.schemaMigrator.getMySQLViews(ctx)
	if err != nil {
		return nil, fmt.Errorf("discover views: %w", err)
	}
	for _, view := range views {
		report.Views = append(report.Views, view.ViewName)
	}
	functions, err := m.schemaMigrator.getMySQLFunctions(ctx)
	if err != nil {
		return nil, fmt.Errorf("discover functions: %w", err)
	}
	for _, function := range functions {
		report.Functions = append(report.Functions, function.FunctionName)
	}

	report.ManualWorkItems = len(report.Views) + len(report.Functions)
	report.IssueCount = len(report.Issues)
	for _, table := range report.Tables {
		report.ManualWorkItems += len(table.Triggers)
		for _, fk := range table.ForeignKeys {
			if fk.SourceMissing {
				report.ManualWorkItems++
			}
		}
		report.IssueCount += len(table.Issues)
		for _, column := range table.Columns {
			if column.Issue != "" {
				report.IssueCount++
			}
		}
		for _, index := range table.Indexes {
			if index.Issue != "" {
				report.IssueCount++
			}
		}
	}
	return report, nil
}

func (m *Migrator) dryRunTable(ctx context.Context, table string, engine string, sourceTables map[string]string) TableDryRun {
	tableReport := TableDryRun{Name: table}

	if err := validatePostgresIdentifier(table, "source table name"); err != nil {
		tableReport.Issues = append(tableReport.Issues, err.Error())
	}
	if !strings.EqualFold(engine, "InnoDB") {
		tableReport.Issues = append(tableReport.Issues, fmt.Sprintf("storage engine %q cannot provide a consistent resumable snapshot; convert the table to InnoDB", engine))
	}

	var estimatedRows sql.NullInt64
	if err := m.sourceQueryer(ctx).QueryRowContext(ctx, `
		SELECT TABLE_ROWS
		FROM INFORMATION_SCHEMA.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?
	`, table).Scan(&estimatedRows); err != nil {
		tableReport.Issues = append(tableReport.Issues, fmt.Sprintf("estimate rows: %v", err))
	}
	tableReport.EstimatedRows = estimatedRows.Int64

	columns, err := m.getMySQLTableStructure(ctx, table)
	if err != nil {
		tableReport.Issues = append(tableReport.Issues, fmt.Sprintf("read table structure: %v", err))
		return tableReport
	}

	// One table scan covers the empty-marker check for every enum column.
	var enumColumnNames []string
	for _, col := range columns {
		if strings.Contains(strings.ToLower(col.Type), "enum") {
			enumColumnNames = append(enumColumnNames, col.Name)
		}
	}
	markerCounts, err := m.sourceEnumEmptyMarkerCounts(ctx, table, enumColumnNames)
	if err != nil {
		tableReport.Issues = append(tableReport.Issues, fmt.Sprintf("inspect enum columns: %v", err))
	}

	// One more scan counts MySQL zero dates per temporal column, so the run
	// never fails mid-copy on a NOT NULL zero date the plan could have shown.
	zeroDateCounts, err := m.sourceZeroDateCounts(ctx, table, columns)
	if err != nil {
		tableReport.Issues = append(tableReport.Issues, fmt.Sprintf("inspect temporal columns: %v", err))
	}
	for _, col := range columns {
		count := zeroDateCounts[col.Name]
		if count == 0 {
			continue
		}
		if col.Nullable {
			tableReport.Warnings = append(tableReport.Warnings, fmt.Sprintf("column %s has %d rows holding MySQL's zero date ('0000-00-00 ...'); PostgreSQL cannot represent them and they will migrate as NULL", col.Name, count))
		} else {
			tableReport.Issues = append(tableReport.Issues, fmt.Sprintf("column %s has %d rows holding MySQL's zero date but is NOT NULL; the migration will stop on them — set real dates or make the column nullable first (find them with: SELECT * FROM %s WHERE %s LIKE '0000-00-00%%')", col.Name, count, quoteMySQLIdentifier(table), quoteMySQLIdentifier(col.Name)))
		}
	}

	buildable := true
	for _, col := range columns {
		columnReport := ColumnDryRun{
			Name:      col.Name,
			MySQLType: col.Type,
			Nullable:  col.Nullable,
		}
		_, pgType, err := m.buildColumnDefinition(table, col)
		columnReport.PostgresType = pgType
		if err != nil {
			columnReport.Issue = err.Error()
			buildable = false
		}
		tableReport.Columns = append(tableReport.Columns, columnReport)

		if strings.Contains(strings.ToLower(col.Extra), "on update current_timestamp") {
			tableReport.Warnings = append(tableReport.Warnings, fmt.Sprintf("column %s uses MySQL ON UPDATE CURRENT_TIMESTAMP; the equivalent PostgreSQL trigger DDL will be saved to the manual work file", col.Name))
		}
		if isGeneratedColumn(col.Extra) {
			tableReport.Warnings = append(tableReport.Warnings, fmt.Sprintf("column %s is a MySQL generated column (%s); it will migrate as a plain column holding the computed snapshot values, with a translation template in the manual work file", col.Name, col.GenerationExpression))
		}
		if col.UUIDDemotionReason != "" {
			tableReport.Warnings = append(tableReport.Warnings, fmt.Sprintf("column %s looks like a UUID column by shape, but %s", col.Name, col.UUIDDemotionReason))
		}

		if strings.Contains(strings.ToLower(col.Type), "enum") {
			if values := m.extractEnumValues(col.Type); len(values) > 0 {
				if markerCounts[col.Name] > 0 && !slices.Contains(values, "") {
					tableReport.Warnings = append(tableReport.Warnings, fmt.Sprintf("enum column %s has %d rows holding MySQL's empty-string invalid-enum marker (invalid values inserted outside strict SQL mode); '' will be added to the PostgreSQL enum type so the data copies losslessly", col.Name, markerCounts[col.Name]))
					values = append([]string{""}, values...)
				}
				tableReport.EnumTypes = append(tableReport.EnumTypes, EnumTypeDryRun{
					Name:   postgresEnumTypeName(table, col.Name),
					Values: values,
				})
			}
		}
	}
	if buildable {
		if createSQL, err := m.buildCreateTableSQL(table, columns); err == nil {
			tableReport.CreateSQL = createSQL
		}
	}

	primaryKey, err := m.getMySQLPrimaryKeyColumns(ctx, table)
	if err != nil {
		tableReport.Issues = append(tableReport.Issues, fmt.Sprintf("read primary key: %v", err))
	}
	tableReport.PrimaryKey = primaryKey
	tableReport.Resumable = len(primaryKey) > 0
	if !tableReport.Resumable {
		tableReport.Warnings = append(tableReport.Warnings, "table has no primary key; it will be copied in one streaming pass and cannot be resumed after interruption")
	}

	indexes, err := m.schemaMigrator.getMySQLIndexes(ctx, table)
	if err != nil {
		tableReport.Issues = append(tableReport.Issues, fmt.Sprintf("read indexes: %v", err))
	}
	for _, index := range indexes {
		if index.IsPrimary {
			continue
		}
		indexReport := IndexDryRun{
			Name:   postgresIndexName(table, index.IndexName),
			Unique: index.IsUnique,
		}
		if index.IndexType != "" && !strings.EqualFold(index.IndexType, "BTREE") {
			indexReport.Issue = fmt.Sprintf("unsupported MySQL index type %s", index.IndexType)
		}
		for _, column := range index.Columns {
			indexReport.Columns = append(indexReport.Columns, column.Name)
			if _, err := postgresIndexColumnSQL(column); err != nil && indexReport.Issue == "" {
				indexReport.Issue = fmt.Sprintf("column %s: %v", column.Name, err)
			}
		}
		tableReport.Indexes = append(tableReport.Indexes, indexReport)
	}

	foreignKeys, err := m.schemaMigrator.getMySQLForeignKeys(ctx, table)
	if err != nil {
		tableReport.Issues = append(tableReport.Issues, fmt.Sprintf("read foreign keys: %v", err))
	}
	for _, fk := range foreignKeys {
		_, referencedExists := sourceTables[strings.ToLower(fk.ReferencedTable)]
		plan := ForeignKeyPlan{
			Name:              boundedPostgresObjectName(fk.ConstraintName, fk.TableName+"\x00"+fk.ConstraintName),
			Columns:           fk.ColumnNames,
			ReferencedTable:   fk.ReferencedTable,
			ReferencedColumns: fk.ReferencedColumns,
			OnDelete:          fk.OnDelete,
			OnUpdate:          fk.OnUpdate,
			SourceMissing:     !referencedExists,
		}
		if plan.SourceMissing {
			plan.SQL = m.schemaMigrator.buildForeignKeySQL(fk, plan.Name)
			tableReport.Warnings = append(tableReport.Warnings, fmt.Sprintf("foreign key %s references table %s, which does not exist in the source database (partial backup?); the constraint will be skipped and its DDL saved for manual work", fk.ConstraintName, fk.ReferencedTable))
		} else {
			orphans, err := m.schemaMigrator.countOrphanRows(ctx, fk)
			if err != nil {
				tableReport.Issues = append(tableReport.Issues, fmt.Sprintf("check foreign key %s for orphaned rows: %v", fk.ConstraintName, err))
			} else if orphans > 0 {
				plan.OrphanRows = orphans
				plan.SQL = m.schemaMigrator.buildForeignKeySQL(fk, plan.Name)
				tableReport.Warnings = append(tableReport.Warnings, fmt.Sprintf("foreign key %s has %d child rows with no matching parent in %s; the constraint will be skipped and its DDL saved for manual work until the rows are repaired", fk.ConstraintName, orphans, fk.ReferencedTable))
			}
		}
		tableReport.ForeignKeys = append(tableReport.ForeignKeys, plan)
	}

	triggers, err := m.schemaMigrator.getMySQLTriggers(ctx, table)
	if err != nil {
		tableReport.Issues = append(tableReport.Issues, fmt.Sprintf("read triggers: %v", err))
	}
	for _, trigger := range triggers {
		tableReport.Triggers = append(tableReport.Triggers, trigger.TriggerName)
	}

	return tableReport
}

// Render prints the report as a human-readable plan.
func (r *DryRunReport) Render(writeLine func(string)) {
	writeLine(fmt.Sprintf("Dry run: %s → PostgreSQL schema %q (no changes were made)", r.SourceDatabase, r.TargetSchema))
	writeLine(fmt.Sprintf("Tables selected: %d of %d; load method: %s", r.SelectedTables, r.TotalTables, r.LoadMethod))
	writeLine("")

	for _, table := range r.Tables {
		header := fmt.Sprintf("📋 %s (~%d rows)", table.Name, table.EstimatedRows)
		if len(table.PrimaryKey) > 0 {
			header += fmt.Sprintf(", primary key (%s), resumable", strings.Join(table.PrimaryKey, ", "))
		} else {
			header += ", no primary key, not resumable"
		}
		writeLine(header)

		for _, column := range table.Columns {
			line := fmt.Sprintf("   • %s: %s → %s", column.Name, column.MySQLType, column.PostgresType)
			if column.PostgresType == "" {
				line = fmt.Sprintf("   • %s: %s → (unsupported)", column.Name, column.MySQLType)
			}
			if column.Issue != "" {
				line += " ❌ " + column.Issue
			}
			writeLine(line)
		}
		for _, enum := range table.EnumTypes {
			writeLine(fmt.Sprintf("   • enum type %s (%s)", enum.Name, strings.Join(enum.Values, ", ")))
		}
		for _, index := range table.Indexes {
			kind := "index"
			if index.Unique {
				kind = "unique index"
			}
			line := fmt.Sprintf("   • %s %s (%s)", kind, index.Name, strings.Join(index.Columns, ", "))
			if index.Issue != "" {
				line += " ❌ " + index.Issue
			}
			writeLine(line)
		}
		for _, fk := range table.ForeignKeys {
			line := fmt.Sprintf("   • foreign key %s (%s) → %s (%s)", fk.Name, strings.Join(fk.Columns, ", "), fk.ReferencedTable, strings.Join(fk.ReferencedColumns, ", "))
			if fk.SourceMissing {
				line += " ⚠️ skipped: referenced table missing from source"
			}
			writeLine(line)
		}
		for _, trigger := range table.Triggers {
			writeLine(fmt.Sprintf("   ⚠️ trigger %s requires manual PostgreSQL conversion", trigger))
		}
		for _, warning := range table.Warnings {
			writeLine("   ⚠️ " + warning)
		}
		for _, issue := range table.Issues {
			writeLine("   ❌ " + issue)
		}
		writeLine("")
	}

	for _, view := range r.Views {
		writeLine(fmt.Sprintf("⚠️ view %s requires manual PostgreSQL conversion", view))
	}
	for _, function := range r.Functions {
		writeLine(fmt.Sprintf("⚠️ function %s requires manual PostgreSQL conversion", function))
	}
	for _, issue := range r.Issues {
		writeLine("❌ " + issue)
	}

	writeLine("")
	if r.IssueCount == 0 {
		writeLine(fmt.Sprintf("✅ Plan is clean: %d tables ready; %d objects need manual conversion.", len(r.Tables), r.ManualWorkItems))
	} else {
		writeLine(fmt.Sprintf("❌ Plan has %d blocking issues across %d tables; resolve them before migrating. %d objects need manual conversion.", r.IssueCount, len(r.Tables), r.ManualWorkItems))
	}
}
