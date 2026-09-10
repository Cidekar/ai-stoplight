//go:build darwin

package service

import (
	"encoding/xml"
	"io"
	"strings"
)

// generatedArgv reads ProgramArguments back out of the plist this package
// writes. Decoding the real XML, rather than matching on the strings that
// built it, means a malformed plist fails here instead of passing a text
// comparison and failing in launchd.
//
// The plist dict is a flat run of alternating <key> and value elements, so
// the array wanted is the first <array> that follows the ProgramArguments
// key. This is walked with a token reader rather than unmarshalled into a
// struct: a `[][]string` field on "dict>array>string" collects one slice per
// <string> element rather than one per <array>, which quietly returns only
// the first argument and makes a check on the rest pass against anything.
func generatedArgv(binPath string) ([]string, bool) {
	l := &launchd{plistPath: "plist", logFile: "log", uid: 501}

	dec := xml.NewDecoder(strings.NewReader(string(l.plist(binPath))))

	// wantKey is set once the ProgramArguments key is read, so the next
	// <array> encountered is the one to collect.
	wantKey := false
	inArray := false
	var (
		args []string
		text strings.Builder
		cur  string
	)

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false
		}

		switch t := tok.(type) {
		case xml.StartElement:
			text.Reset()
			cur = t.Name.Local
			if t.Name.Local == "array" && wantKey {
				inArray = true
				args = []string{}
			}
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			switch t.Name.Local {
			case "key":
				if cur == "key" && strings.TrimSpace(text.String()) == "ProgramArguments" {
					wantKey = true
				}
			case "string":
				if inArray {
					args = append(args, text.String())
				}
			case "array":
				if inArray {
					// The array that follows the key is complete.
					return args, true
				}
			}
			text.Reset()
		}
	}
	return nil, false
}
