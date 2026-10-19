package solar

import _ "embed"

const AstralVersion = "2.2"
const AstralCommit = "3282d7b437349bc035c8a7dff5c27ec9997a2cdf"
const HAProfileCommit = "abe1fdd7cd6b4de1715c87b6fd87d92699514ef9"

// astralLicense preserves the license distributed with the ported source.
// Copyright 2009-2019 Simon Kennedy; adapted to Go for Homeward in 2026.
//
//go:embed LICENSE.astral
var astralLicense string

func LicenseText() string { return astralLicense }
