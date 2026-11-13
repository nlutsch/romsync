package server

import "testing"

func TestFriendlyTitles(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"Super Mario World (U) [!]", "Super Mario World"},
		{"Zelda (USA) (Rev A) [b1]", "Zelda"},
		{"Pokemon (Europe) (En,Fr,De) [T+Eng]", "Pokemon"},
		{"Sonic_the_Hedgehog (World) (v1.2)", "Sonic the Hedgehog"},
		{"Game (Beta 2)", "Game"},
		{"Game (The Lost Levels)", "Game (The Lost Levels)"},
		{"Game (Disc 1)", "Game (Disc 1)"},
		{" [!] ", " [!] "},
	} {
		t.Run(tc.input, func(t *testing.T) {
			if got := friendlyTitle(tc.input); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
