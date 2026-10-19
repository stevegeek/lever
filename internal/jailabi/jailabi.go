// Package jailabi holds the in-jail compatibility version: the number the
// agent image is stamped with (its lever_jail_abi label) and that `lever
// doctor` compares with the host lever.
//
// The release version is the wrong thing to compare: a release that changes
// no in-jail code (lever-agent, lever-manager, lever-tool-db, the scion
// pre-start hook, the image Dockerfile, anything they import, or the Go
// version) still bumps it. JailABI changes only when the code baked into the
// image changes.
//
// # Bumping it
//
// The guard test in this package (TestJailFingerprint) hashes the in-jail
// sources and compares the hash with fingerprint.txt. When it fails after a
// change to in-jail code:
//
//  1. add one to JailABI below;
//  2. run `go test ./internal/jailabi -run TestJailFingerprint -update`;
//  3. commit both files with the change.
//
// The update refuses to record a new hash under the JailABI already recorded,
// so the bump cannot be skipped. A change that keeps the code but changes
// what it means on the wire may bump JailABI by hand; run the update after it
// too, so fingerprint.txt records the new number.
//
// The Makefile reads this constant with sed (keep the `const JailABI = N`
// line as it is) and passes it to the image build as the lever_jail_abi
// label.
package jailabi

// JailABI is the in-jail compatibility version. See the package doc for how
// and when to bump it.
const JailABI = 7

// Label is the image label that carries JailABI (image/lever-claude/Dockerfile).
const Label = "lever_jail_abi"
