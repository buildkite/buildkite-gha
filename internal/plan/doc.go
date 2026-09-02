// Package plan defines Job, the serialized handoff from compiler to runtime. A
// Job contains a program.Program plus the workflow and event identity,
// dependencies, permissions, action locks, and runtime version needed to
// validate and run it.
package plan
