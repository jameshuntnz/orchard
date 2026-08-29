// Package version records which release this source tree is.
package version

// Current is rewritten by the release workflow before it builds, and committed back to
// main only for a stable release. A dev or rc build stamps it without committing, so the
// binary reports what it is while main keeps recording the last finished release.
//
// A build from a working tree therefore claims the last stable version. That is close
// enough to true — it is that release plus whatever has landed since — and
// deploy/install.sh derives a dev version instead, so a hand-installed binary sorts
// correctly against what the release feed publishes.
const Current = "0.1.0"
