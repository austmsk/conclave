// Package contracts holds the types and interfaces shared across Conclave.
//
// Everything else in the codebase is built behind these definitions, which is
// what allows independent packages to be written in parallel without
// colliding. Changes here ripple outward, so they are reviewed by hand and
// never made as a side effect of another change. Agent sessions must flag any
// proposed change to this package rather than making it directly.
//
// This package contains no behaviour: no I/O, no logging, no side effects.
// Only types, interfaces, constants, and pure validation helpers. It must not
// import any other Conclave package.
package contracts
