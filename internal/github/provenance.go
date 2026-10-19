package github

// Source is where a piece of GitHub text came from, decided from the
// payload's author fields alone (FR-SEC28). Text never decides its own
// source: a body that says "posted by the owner" is still whatever its
// author fields say.
type Source string

const (
	// SourceOwner is the owner's own account, acting directly.
	SourceOwner Source = "owner"
	// SourcePlatform is this App: Conclave's own output.
	SourcePlatform Source = "platform"
	// SourceBot is any other App or bot account. Bot text relays text
	// authored elsewhere, so it is never trusted.
	SourceBot Source = "bot"
	// SourceStranger is any other human, organisation, or account type.
	SourceStranger Source = "stranger"
)

// Author is the identity GitHub attaches to an issue, a comment, or an
// event sender. Login is deliberately absent: it changes, and nothing may
// decide on it.
type Author struct {
	// ID is the account's numeric user ID.
	ID int64
	// Type is the account type as GitHub reports it: "User", "Bot",
	// "Organization", "Mannequin".
	Type string
	// AppID is performed_via_github_app.id, or 0 when the action was not
	// performed through an App.
	AppID int64
}

// Classify decides an author's Source. Order matters: the App flag comes
// first, so the owner's own account acting through an App is a bot, not
// the owner. Anything unrecognised is a stranger, never the owner.
func Classify(a Author, ownerID, appID int64) Source {
	switch {
	case a.AppID != 0 && a.AppID == appID:
		return SourcePlatform
	case a.AppID != 0 || a.Type == "Bot":
		return SourceBot
	case a.Type == "User" && ownerID > 0 && a.ID == ownerID:
		return SourceOwner
	default:
		return SourceStranger
	}
}
