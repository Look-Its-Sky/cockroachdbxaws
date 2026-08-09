// Package outage holds the CockroachDB outage scenario operations.md requires
// this service to survive.
//
// The outage is produced by cutting the network to the database rather than by
// stopping it. See internal/testsupport/netgate for why: the CockroachDB test
// container runs an in-memory store, so stopping it would destroy the very data
// this scenario has to show survived.
//
// There is no production code here.
package outage
