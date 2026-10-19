// Package dockertest gives tests a real Docker sandbox: a provider in a
// namespace of its own, the pinned test image, a prepared fixture tree,
// and reaping when the test ends. Tests outside the docker package use it
// to attack tools through the backend the worker actually runs.
package dockertest
