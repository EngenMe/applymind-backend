package applications

import "testing"

func TestTitlesLikelySame(t *testing.T) {
	cases := []struct {
		name  string
		left  string
		right string
		want  bool
	}{
		{
			name:  "identical wording",
			left:  "Backend Engineer",
			right: "Backend Engineer",
			want:  true,
		},
		{
			name:  "punctuation and case only",
			left:  "Senior Backend Engineer (Go)",
			right: "senior backend engineer, go",
			want:  true,
		},
		{
			name:  "company board adds detail",
			left:  "Senior Backend Engineer",
			right: "Senior Backend Engineer — Payments Team",
			want:  true,
		},
		{
			name:  "work arrangement noise ignored",
			left:  "Go Developer (Remote)",
			right: "Go Developer - Full Time, Hybrid",
			want:  true,
		},
		{
			name:  "german gender note ignored",
			left:  "Softwareentwickler Backend (m/w/d)",
			right: "Softwareentwickler Backend",
			want:  true,
		},
		{
			name:  "seniority is not noise",
			left:  "Junior Go Developer",
			right: "Principal Go Developer",
			want:  false,
		},
		{
			name:  "different discipline at the same company",
			left:  "Backend Engineer",
			right: "Product Designer",
			want:  false,
		},
		{
			name:  "one shared word is not enough",
			left:  "Engineering Manager, Platform",
			right: "Data Engineering Intern",
			want:  false,
		},
		{
			name:  "empty title never matches",
			left:  "",
			right: "Backend Engineer",
			want:  false,
		},
		{
			name:  "title of only noise never matches",
			left:  "(m/w/d)",
			right: "Backend Engineer",
			want:  false,
		},
	}

	for _, tc := range cases {
		t.Run(
			tc.name, func(t *testing.T) {
				if got := TitlesLikelySame(tc.left, tc.right); got != tc.want {
					t.Fatalf("TitlesLikelySame(%q, %q) = %v, want %v", tc.left, tc.right, got, tc.want)
				}
				// The question is symmetric; a rule that answers differently
				// depending on which application came first would warn or stay
				// silent by accident.
				if got := TitlesLikelySame(tc.right, tc.left); got != tc.want {
					t.Fatalf("TitlesLikelySame(%q, %q) = %v, want %v (not symmetric)", tc.right, tc.left, got, tc.want)
				}
			},
		)
	}
}
