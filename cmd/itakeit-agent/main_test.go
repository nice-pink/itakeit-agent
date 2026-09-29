package main

import "testing"

func TestAppTokenApp(t *testing.T) {
	for token, want := range map[string]string{"xapp-1-A0C5BA99RSN-1234-abcdef": "A0C5BA99RSN", "xoxb-1-2": "", "xapp": "", "": ""} {
		if got := appTokenApp(token); got != want {
			t.Errorf("appTokenApp(%q) = %q, want %q", token, got, want)
		}
	}
}
