package gate

// GitleaksPin identifies the gitleaks binary the worker trusts.
type GitleaksPin struct {
	Path    string
	Version string
	SHA256  string
}
