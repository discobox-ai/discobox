// Package skillpolicies holds the check that the skills this repository ships
// ask for credentials as Discobox takes them: every request a skill shows
// carries a policy that parses and validates, written in the canonical
// sentences a lead compares word for word, and a worker's policy falls within
// the delegation its lead asks for (#312). It has no code of its own.
package skillpolicies
