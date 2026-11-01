// Package alert records and delivers platform alerts (FR-SEC31).
//
// Its single responsibility is one alert's path: write it to the local
// append-only log on the central machine, then deliver it as an issue in
// the platform's private operations repository, then record whether
// delivery worked. The log comes first, so the record survives a GitHub
// path that is down or compromised.
//
// An alert carries identifiers and fixed, platform-written text only:
// never a repository's content, an issue body, a diff, a transcript, or a
// secret. Each constructor takes typed identifiers and composes the text
// itself, and the one free-text field, the owner's halt reason, is
// redacted and bounded.
//
// Delivery is deduplicated on GitHub itself: an open issue the App opened
// whose body carries the alert's key line is updated with a comment
// rather than duplicated, and an occurrence line on the issue or a
// comment makes a retried delivery a no-op. There is no alerts table.
package alert
