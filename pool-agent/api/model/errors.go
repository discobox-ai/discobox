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

// ErrorTypeSandboxImageUnavailable is the RFC 7807 `type` a pool agent sets on
// the 422 it returns when it cannot obtain the image a sandbox is pinned to: the
// pinned image is not on the pool and its reference no longer names it, or the
// registry has no image by that reference.
//
// The control plane records it as the reason a sandbox failed, which is what
// lets a client say so plainly and offer the upgrade that re-pins the sandbox to
// its harness's current image. Retrying does not help; new intent does. It is
// deliberately not a 409, which an older control plane reads as "already
// exists" and settles as a healthy sandbox.
const ErrorTypeSandboxImageUnavailable = "https://discobox.ai/errors/sandbox-image-unavailable"
