package logship

// Type is a kind of log WPGenie can ship. Each is collected one of three
// ways:
//   - tailed: Vector reads the file where it is (the file's owner rotates it);
//   - spooled: the daemon writes it to the spool (it truncates the file it
//     reads, keeps it only in memory, or it's its own output), and Vector
//     ships and deletes the spool's files;
//   - exported: the daemon reads new rows of one of its tables after a
//     cursor and spools them.
type Type struct {
	Name    string `json:"name"`
	Collect string `json:"collect"` // tailed | spooled | exported
	Default bool   `json:"default"`
}

const (
	TypeAccess        = "access"
	TypePHPErrors     = "php_errors"
	TypeWAF           = "waf"
	TypeSecurity      = "security"
	TypeDaemon        = "daemon"
	TypeAudit         = "audit"
	TypeJobs          = "jobs"
	TypeAccountEvents = "account_events"
	TypeEmail         = "email"
	TypeMail          = "mail"
	TypeContainers    = "containers"
)

const (
	collectTailed   = "tailed"
	collectSpooled  = "spooled"
	collectExported = "exported"
)

// Types is every kind of log, in the order the panel shows them. The
// containers' own output is off by default: it repeats much of the rest,
// and is the noisiest. The daemon reads it (containers.go) rather than
// Vector: Docker's containers directory also holds every container's
// settings, secrets included, which the shipper has no business seeing.
var Types = []Type{
	{TypeAccess, collectTailed, true},
	{TypePHPErrors, collectSpooled, true},
	{TypeWAF, collectSpooled, true},
	{TypeSecurity, collectSpooled, true},
	{TypeAudit, collectExported, true},
	{TypeJobs, collectExported, true},
	{TypeAccountEvents, collectExported, true},
	{TypeEmail, collectExported, true},
	{TypeMail, collectTailed, true},
	{TypeDaemon, collectSpooled, true},
	{TypeContainers, collectSpooled, false},
}

// TypeByName is the type called name, or nil.
func TypeByName(name string) *Type {
	for i := range Types {
		if Types[i].Name == name {
			return &Types[i]
		}
	}
	return nil
}
