// Package privatefstest widens a file or directory's permissions for a test,
// so an assertion through privatefs.Check proves a writer restricted what it
// wrote rather than that the test's temporary directory happened to be
// private already — which on a default Windows profile it is.
package privatefstest
