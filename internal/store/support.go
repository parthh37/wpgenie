package store

// supportSchema is this feature's migration (appended to migrations in
// store.go). A no-op until the feature's tables land here.
const supportSchema = `SELECT 1;`
