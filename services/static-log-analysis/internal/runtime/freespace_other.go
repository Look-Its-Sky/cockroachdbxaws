//go:build !unix

package runtime

import "errors"

// freeSpace has no portable implementation. Reporting an unbounded amount would
// make the journal's capacity policy silently inoperative on this platform, so
// an error is returned instead. OpenJournal probes once at startup and refuses
// to open the journal, which turns this into a configuration fault an operator
// sees rather than a replica that binds, reports healthy, and then refuses
// every export.
func freeSpace(string) (uint64, error) {
	return 0, errors.New("runtime: free space is not measurable on this platform")
}
