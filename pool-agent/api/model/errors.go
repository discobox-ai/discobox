package model

// ErrorTypeSandboxArchived is the RFC 7807 `type` a pool agent sets on the 409
// it returns for an archived sandbox.
//
// A conflict is not one condition on this API: "the sandbox already exists" and
// "the sandbox is archived" are both 409, and they call for opposite responses
// -- the first means the caller's create already happened, the second means
// nothing will run until someone unarchives it. The status alone cannot tell
// them apart, and the human-readable detail is not a contract, so the type is
// what the control plane matches on (ADR 0022 §5).
const ErrorTypeSandboxArchived = "https://discobox.ai/errors/sandbox-archived"
