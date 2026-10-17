// Package testenv decides what a test does when its environment is missing
// something it needs, such as Docker or git's HTTP backend. Locally the test
// skips; in CI, where CONCLAVE_REQUIRE_INTEGRATION is set, it fails, so a
// security or integration test can never pass by not running.
package testenv
