// Copyright (c) 2026 Neomantra Corp. All rights reserved.
// SPDX-License-Identifier: AGPL-3.0-only WITH LicenseRef-NimbleTerminal-Linking-Exception OR LicenseRef-Neomantra-Commercial

package ntmediabox

import "nimbleterminal.dev/ntconfine"

// HelperPolicy renders the macOS sandbox-exec SBPL profile for pol.
func HelperPolicy(pol Policy) string {
	return ntconfine.HelperPolicy(pol)
}
