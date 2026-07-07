package vault

import "time"

// nowISO returns the current UTC time formatted as ISO-8601 with
// millisecond precision, matching the strftime default used in the
// credentials table's created_at / updated_at defaults.
func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// nowUTC returns time.Now() in UTC for engines (Postgres) that store
// timestamps natively.
func nowUTC() time.Time {
	return time.Now().UTC()
}
