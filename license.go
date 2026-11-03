// Copyright (c) 2026 Neomantra Corp. All rights reserved.
// SPDX-License-Identifier: AGPL-3.0-only WITH LicenseRef-NimbleTerminal-Linking-Exception OR LicenseRef-Neomantra-Commercial

package ntmediabox

import (
	"crypto/ed25519"
	"time"

	"nimbleterminal.dev/ntconfine"
)

const (
	// LicenseEnvVar is the environment variable holding a signed Neomantra Corp.
	// commercial OEM or enterprise runtime license key.
	LicenseEnvVar = ntconfine.LicenseEnvVar

	// LicenseTokenPrefix identifies a v1 Ed25519-signed Neomantra license key.
	LicenseTokenPrefix = ntconfine.LicenseTokenPrefix

	// DefaultLicensePublicKeyHex is Neomantra Corp.'s default Ed25519 verifying key.
	DefaultLicensePublicKeyHex = ntconfine.DefaultLicensePublicKeyHex
)

var (
	// ErrNoLicenseKey indicates NIMBLE_LICENSE_KEY is not set in the environment.
	ErrNoLicenseKey = ntconfine.ErrNoLicenseKey
	// ErrInvalidLicenseKey indicates a malformed or tampered license token.
	ErrInvalidLicenseKey = ntconfine.ErrInvalidLicenseKey
	// ErrExpiredLicenseKey indicates a valid signature whose expiration time has passed.
	ErrExpiredLicenseKey = ntconfine.ErrExpiredLicenseKey
	// ErrLicenseScope indicates the license key does not cover the requested module.
	ErrLicenseScope = ntconfine.ErrLicenseScope
)

// LicenseClaims holds the verified payload of a signed NIMBLE_LICENSE_KEY.
type LicenseClaims = ntconfine.LicenseClaims

// DefaultLicensePublicKey returns the built-in Neomantra Corp. Ed25519 public key.
func DefaultLicensePublicKey() ed25519.PublicKey {
	return ntconfine.DefaultLicensePublicKey()
}

// SignLicenseKey serializes claims as JSON and signs them with priv.
func SignLicenseKey(claims LicenseClaims, priv ed25519.PrivateKey) (string, error) {
	return ntconfine.SignLicenseKey(claims, priv)
}

// VerifyLicenseKey verifies token against DefaultLicensePublicKey at the current time.
func VerifyLicenseKey(token string) (*LicenseClaims, error) {
	return ntconfine.VerifyLicenseKey(token)
}

// VerifyLicenseKeyWithKey verifies token against pub at now.
func VerifyLicenseKeyWithKey(token string, pub ed25519.PublicKey, now time.Time) (*LicenseClaims, error) {
	return ntconfine.VerifyLicenseKeyWithKey(token, pub, now)
}

// CheckCommercialLicense reads NIMBLE_LICENSE_KEY from the process environment,
// verifies its Ed25519 signature and expiration, and checks that module is in scope.
func CheckCommercialLicense(module string) (*LicenseClaims, error) {
	return ntconfine.CheckCommercialLicense(module)
}

// CheckCommercialLicenseWithKey is CheckCommercialLicense with an explicit
// verifying key and clock.
func CheckCommercialLicenseWithKey(module string, pub ed25519.PublicKey, now time.Time) (*LicenseClaims, error) {
	return ntconfine.CheckCommercialLicenseWithKey(module, pub, now)
}

// HasCommercialLicense reports whether a valid, unexpired NIMBLE_LICENSE_KEY
// covering module is present in the environment.
func HasCommercialLicense(module string) bool {
	return ntconfine.HasCommercialLicense(module)
}
