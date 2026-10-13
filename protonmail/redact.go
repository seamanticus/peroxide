package protonmail

import (
	"bytes"
	"encoding/json"
	"strings"
)

// RedactedValue replaces credentials in debug logs.
const RedactedValue = "<API key removed for security>"

// redactedJSONFields lists the JSON object keys whose values must never end up
// in debug logs: credentials, SRP material, session tokens, key material and
// human verification tokens.
var redactedJSONFields = map[string]bool{
	"accesstoken":            true,
	"refreshtoken":           true,
	"humanverificationtoken": true,
	"token":                  true,
	"clientephemeral":        true,
	"clientproof":            true,
	"serverephemeral":        true,
	"serverproof":            true,
	"salt":                   true,
	"keysalt":                true,
	"privatekey":             true,
	"password":               true,
	"loginpassword":          true,
	"mailboxpassword":        true,
	"twofactorcode":          true,
	"cookie":                 true,
	// Cleartext session keys, sent when mailing recipients outside Proton.
	"key": true,
}

// redactJSON returns b with the values of sensitive fields replaced. If b isn't
// valid JSON it is replaced entirely, as we can't tell what it holds.
func redactJSON(b []byte) []byte {
	// A decoder alone would accept, and drop, data after the first value.
	if !json.Valid(b) {
		return []byte(`"` + RedactedValue + `"`)
	}

	dec := json.NewDecoder(bytes.NewReader(b))
	// Keep numbers as sent, float64 would mangle large ones.
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return []byte(`"` + RedactedValue + `"`)
	}

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	// Logs aren't HTML: keep RedactedValue and message contents readable.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(redactValue(v)); err != nil {
		return []byte(`"` + RedactedValue + `"`)
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n"))
}

// redactValueOf marshals v to JSON and redacts its sensitive fields.
func redactValueOf(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`"` + RedactedValue + `"`)
	}
	return redactJSON(b)
}

func redactValue(v interface{}) interface{} {
	switch v := v.(type) {
	case map[string]interface{}:
		for k, sub := range v {
			if redactedJSONFields[strings.ToLower(k)] {
				if sub != nil {
					v[k] = RedactedValue
				}
				continue
			}
			v[k] = redactValue(sub)
		}
		return v
	case []interface{}:
		for i, sub := range v {
			v[i] = redactValue(sub)
		}
		return v
	default:
		return v
	}
}
