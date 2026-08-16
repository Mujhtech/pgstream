package migrator

import (
	"context"
	"fmt"
	"testing"
)

// benchMigrator builds a Migrator with a pre-warmed validation metadata cache
// so the transform hot path can run without a database.
func benchMigrator() (*Migrator, []string) {
	columns := []string{"id", "userId", "reference", "body", "amount", "isBlocked", "status", "createdAt", "deletedAt"}
	info := map[string]postgresColumnInfo{
		"id":        {name: "id", dataType: "uuid", udtName: "uuid", isNullable: false},
		"userid":    {name: "userId", dataType: "character varying", udtName: "varchar", isNullable: false},
		"reference": {name: "reference", dataType: "character varying", udtName: "varchar", isNullable: false},
		"body":      {name: "body", dataType: "text", udtName: "text", isNullable: true},
		"amount":    {name: "amount", dataType: "numeric", udtName: "numeric", isNullable: false},
		"isblocked": {name: "isBlocked", dataType: "boolean", udtName: "bool", isNullable: false},
		"status":    {name: "status", dataType: "USER-DEFINED", udtName: "bench_status_enum", isNullable: false},
		"createdat": {name: "createdAt", dataType: "timestamp without time zone", udtName: "timestamp", isNullable: false},
		"deletedat": {name: "deletedAt", dataType: "timestamp without time zone", udtName: "timestamp", isNullable: true},
	}
	migrator := &Migrator{
		schemaName: "bench",
		metadataCache: map[string]*tableValidationMetadata{
			"bench.events": {
				columns: info,
				enumValuesByColumn: map[string]map[string]struct{}{
					"status": {"ACTIVE": {}, "PENDING": {}, "FAILED": {}},
				},
			},
		},
	}
	return migrator, columns
}

// benchBatch fabricates rows shaped like the MySQL driver produces them:
// []byte for text-ish values, int64 for tinyint booleans, nil for NULLs.
func benchBatch(rows int) [][]any {
	batch := make([][]any, rows)
	for i := range batch {
		var deleted any
		if i%10 == 0 {
			deleted = []byte("2025-07-19 08:08:54")
		}
		batch[i] = []any{
			[]byte(fmt.Sprintf("af96bcec-ed20-4869-8d76-bf3dfcfc%04d", i%10000)),
			[]byte("41854516-15e1-4742-905f-7c6a880cfab2"),
			[]byte(fmt.Sprintf("ref-%d", i)),
			[]byte(`{"message":"payload body with some text in it","n":12345}`),
			[]byte("129846.5101234567890000"),
			int64(i % 2),
			[]byte("ACTIVE"),
			[]byte("2025-07-19 08:08:54"),
			deleted,
		}
	}
	return batch
}

func BenchmarkTransformBatch(b *testing.B) {
	migrator, columns := benchMigrator()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		batch := benchBatch(1000)
		b.StartTimer()
		if _, err := migrator.validateAndTransformData(ctx, "events", columns, batch); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBuildPostgresInsert(b *testing.B) {
	columns := []string{"id", "userId", "reference", "body", "amount", "isBlocked", "status", "createdAt", "deletedAt"}
	batch := benchBatch(1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := buildPostgresInsert("bench", "events", columns, []string{"id"}, batch); err != nil {
			b.Fatal(err)
		}
	}
}
