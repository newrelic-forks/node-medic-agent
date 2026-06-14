// Package slack carries the small Block Kit URL builder
// (BuildCaseURL) that the controller imports to format the
// "View full diagnosis" button URL. The Slack post path stays in
// internal/nodemedic/notifier/ — this package only owns the URL
// shape so it lives next to the UI's route table. Real code lands
// in Phase 3 (T027).
package slack
