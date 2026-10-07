package compliance

// Perm 是权限点。
type Perm string

const (
	PermIngest Perm = "ingest"
	PermClose  Perm = "close"
	PermReopen Perm = "reopen"
	PermAdjust Perm = "adjust"
	PermRule   Perm = "rule"
	PermRead   Perm = "read"
	PermAudit  Perm = "audit"
)

// User 是一个被认证的调用方及其权限点集合。
type User struct {
	Name  string
	Perms map[Perm]bool
}

// Authorizer 负责权限校验。
type Authorizer interface {
	Authorize(u User, p Perm) bool
}

type staticAuthorizer struct{}

func (staticAuthorizer) Authorize(u User, p Perm) bool {
	return u.Perms != nil && u.Perms[p]
}

// NewStaticAuthorizer 构造一个基于用户自带权限点的鉴权器。
func NewStaticAuthorizer() Authorizer { return staticAuthorizer{} }

// 预置角色的权限点集合。
var (
	AdminRole    = []Perm{PermIngest, PermClose, PermReopen, PermAdjust, PermRule, PermRead, PermAudit}
	OperatorRole = []Perm{PermIngest, PermClose, PermAdjust, PermRead}
	ReaderRole   = []Perm{PermRead}
	AuditorRole  = []Perm{PermRead, PermAudit}
)

// NewUser 按显式权限点集合创建用户。
func NewUser(name string, perms ...Perm) User {
	m := make(map[Perm]bool, len(perms))
	for _, p := range perms {
		m[p] = true
	}
	return User{Name: name, Perms: m}
}

// NewAdmin / NewOperator / NewReader / NewAuditor 按预置角色创建用户。
func NewAdmin(name string) User    { return NewUser(name, AdminRole...) }
func NewOperator(name string) User { return NewUser(name, OperatorRole...) }
func NewReader(name string) User   { return NewUser(name, ReaderRole...) }
func NewAuditor(name string) User  { return NewUser(name, AuditorRole...) }
