/*
Copyright 2026 The New Relic Container Fabric authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package slack

import (
	"fmt"
	"strings"
)

// BuildCaseURL joins a UI base URL with an NHD name into the URL the
// "View full diagnosis" button points at. Lives in this package
// (rather than internal/nodemedic/notifier/) because the URL shape
// belongs to the UI's route table even though the caller is the
// controller's Slack builder.
//
// Spec 001's NHD-name format is path-safe by construction (only
// [a-z0-9-]), so this helper does NOT URL-encode nhdName. If the name
// format ever grows URL-unsafe characters, this function MUST add
// encoding (see Constitution Article II.2 for the cross-scope
// coordination rule). The Block Kit builder tests document the
// current invariant.
func BuildCaseURL(uiBaseURL, nhdName string) (string, error) {
	if uiBaseURL == "" {
		return "", fmt.Errorf("BuildCaseURL: uiBaseURL is required")
	}
	if nhdName == "" {
		return "", fmt.Errorf("BuildCaseURL: nhdName is required")
	}
	base := strings.TrimRight(uiBaseURL, "/")
	return base + "/cases/" + nhdName, nil
}
