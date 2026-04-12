package main

import (
	"strings"
	"unicode"
)

func ownerSantise(owner string) string {

	owner = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			return r
		}
		return -1 // drop character
	}, owner)

	return owner[:min(40, len(owner))]
}

func santise(owner string) string {

	owner = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == ' ' {
			return r
		}
		return -1 // drop character
	}, owner)

	return owner[:min(40, len(owner))]
}
