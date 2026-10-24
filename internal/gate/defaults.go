package gate

// defaults.go holds the floors every repository's policy starts from.
// gates.yaml adds to them and can never remove any (FR-SEC8, FR-SEC25).
// Pattern syntax is described in match.go: a name with no inner "/"
// matches at any depth, and a trailing "/" means a directory.

// defaultProtectedPaths fail the protected_paths gate when changed: CI
// configuration, code ownership, and the secret scanner's own
// configuration. Milestone 1 has no owner approval, so any change fails.
var defaultProtectedPaths = []string{
	".github/",
	".gitlab-ci.yml",
	".circleci/",
	"Jenkinsfile",
	".buildkite/",
	"azure-pipelines.yml",
	".travis.yml",
	"CODEOWNERS",
	".gitleaks*",
}

// defaultExecutionSurfacePaths flag the execution_surface gate: files
// that run code at install, build or commit time, or reconfigure the
// owner's AI tooling. A flag passes the gate and forces clean_room.
var defaultExecutionSurfacePaths = []string{
	// Package manifests, lockfiles and build files. The whole of
	// package.json is surface, which covers its scripts without
	// diffing JSON.
	"package.json",
	"package-lock.json",
	"npm-shrinkwrap.json",
	"yarn.lock",
	"pnpm-lock.yaml",
	"bun.lock",
	"bun.lockb",
	"pyproject.toml",
	"setup.py",
	"setup.cfg",
	"poetry.lock",
	"Pipfile.lock",
	"uv.lock",
	"Cargo.toml",
	"Cargo.lock",
	"build.rs",
	"go.mod",
	"go.sum",
	"pom.xml",
	".mvn/",
	"mvnw",
	"mvnw.cmd",
	"build.gradle*",
	"settings.gradle*",
	"gradle.lockfile",
	"gradlew",
	"gradlew.bat",
	"gradle/wrapper/",
	"Gemfile",
	"Gemfile.lock",
	"*.gemspec",

	// Hook installers.
	".husky/",
	"lefthook.y*ml",
	".lefthook.y*ml",
	".pre-commit-config.yaml",
	".githooks/",

	// Package registry configuration.
	".npmrc",
	".yarnrc*",
	".pnpmfile.cjs",

	// Git attributes name filters and diff drivers; submodules fetch code.
	".gitattributes",
	".gitmodules",

	// Build and environment files.
	"Makefile",
	"*.mk",
	"Dockerfile*",
	"Containerfile",
	".devcontainer/",
	".vscode/",
	".idea/",
	".envrc",

	// AI tooling.
	".mcp.json",
	".claude/",
	".cursor/",
	"CLAUDE.md",
	"AGENTS.md",
}

// Git modes the static gates look for.
const (
	modeExecutable = "100755"
	modeSymlink    = "120000"
	modeGitlink    = "160000"
)
