package version

import "testing"

func TestCompare(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0", "1.0.0", 0},
		{"v1.0.0", "1.0.0", 0},
		{"1.0.0", "1.0.1", -1},
		{"1.0.1", "1.0.0", 1},
		{"1.9", "1.10", -1},
		{"2.0.2", "1.9.9", 1},
		{"2020.05.01", "2020.04.30", 1},
		{"1.0.0-rc1", "1.0.0", -1},
		{"1.0.0rc1", "1.0.0", -1},
		{"1.0.0rc1", "1.0.0-rc1", 0},
		{"1.0.0a1", "1.0.0b1", -1},
		{"1.0.0b2", "1.0.0rc1", -1},
		{"1.0.0rc1", "1.0.0", -1},
		{"1.0.0", "1.0.0.post1", -1},
		{"1.0.0.post2", "1.0.0.post1", 1},
		{"13.4.0", "12.6.5", 1},
	}

	for _, tt := range tests {
		got := Compare(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestIsConstraint(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"latest", false},
		{"1.0.0", false},
		{"v2.0.2", false},
		{">1.0.0", true},
		{">=1.0.0", true},
		{"<2.0.0", true},
		{"<=2.0", true},
		{"!=1.5.0", true},
		{"==1.4.*", true},
		{"~=1.2", true},
		{">=1.0.0,<2.0.0", true},
		{">= 1.0.0", true},
	}

	for _, tt := range tests {
		if got := IsConstraint(tt.in); got != tt.want {
			t.Errorf("IsConstraint(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestSetMatch(t *testing.T) {
	tests := []struct {
		spec string
		ver  string
		want bool
	}{
		{">1.0.0", "1.0.1", true},
		{">1.0.0", "1.0.0", false},
		{">=1.0.0", "1.0.0", true},
		{"<2.0.0", "1.9.9", true},
		{"<2.0.0", "2.0.0", false},
		{"<=2.0", "2.0.0", true},
		{"!=1.5.0", "1.4.0", true},
		{"!=1.5.0", "1.5.0", false},
		{">=1.0.0,<2.0.0", "1.5.0", true},
		{">=1.0.0,<2.0.0", "2.0.0", false},
		{">=1.0.0,<2.0.0", "0.9.0", false},
		{"~=2.2", "2.9.0", true},
		{"~=2.2", "3.0.0", false},
		{"~=1.4.5", "1.4.6", true},
		{"~=1.4.5", "1.5.0", false},
		{"~=1.4.5", "1.4.4", false},
		{"==1.4.*", "1.4.2", true},
		{"==1.4.*", "1.5.0", false},
		{"1.*", "1.9.9", true},
		{"1.*", "0.9.9", false},
		{"1.0.0", "1.0.0", true},
		{"1.0.0", "1.0.1", false},
		{"3.2.0", "v3.2.0", true},
		{"*", "99.0.0", true},
	}

	for _, tt := range tests {
		set, err := ParseSet(tt.spec)
		if err != nil {
			t.Errorf("ParseSet(%q) error: %v", tt.spec, err)
			continue
		}
		v, err := Parse(tt.ver)
		if err != nil {
			t.Errorf("Parse(%q) error: %v", tt.ver, err)
			continue
		}
		if got := set.Match(v); got != tt.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tt.spec, tt.ver, got, tt.want)
		}
	}
}

func TestHighestAndPick(t *testing.T) {
	versions := []string{"1.0.0", "1.5.0", "2.0.0-rc1", "2.0.0", "1.9.9", "not-a-version"}

	highest, err := Highest(versions)
	if err != nil {
		t.Fatalf("Highest: %v", err)
	}
	if highest != "2.0.0" {
		t.Errorf("Highest = %q, want 2.0.0", highest)
	}

	tests := []struct {
		declared string
		want     string
		wantErr  bool
	}{
		{"latest", "2.0.0", false},
		{"", "2.0.0", false},
		{">1.0.0", "2.0.0", false},
		{">=2.0.0", "2.0.0", false},
		{">2.0.0-rc1,<2.0.0", "", true},
		{"~=1.0", "1.9.9", false},
		{"1.5.0", "1.5.0", false},
		{">9.0.0", "", true},
	}

	for _, tt := range tests {
		got, err := Pick(versions, tt.declared)
		if tt.wantErr {
			if err == nil {
				t.Errorf("Pick(%q) expected error, got %q", tt.declared, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Pick(%q) error: %v", tt.declared, err)
			continue
		}
		if got != tt.want {
			t.Errorf("Pick(%q) = %q, want %q", tt.declared, got, tt.want)
		}
	}
}

func TestSatisfies(t *testing.T) {
	tests := []struct {
		declared, disk string
		want           bool
	}{
		{"3.2.0", "3.2.0", true},
		{"3.2.0", "3.2.1", false},
		{"v3.2.0", "3.2.0", true},
		{">1.0.0", "1.5.0", true},
		{">1.0.0", "1.0.0", false},
		{">=1.0.0,<2.0.0", "1.9.9", true},
		{">=1.0.0,<2.0.0", "2.0.0", false},
		{"latest", "1.2.3", true},
		{"", "1.2.3", true},
		{"main", "main", true},
		{"master", "main", false},
		{"~=1.4", "1.9.0", true},
		{"~=1.4", "2.0.0", false},
		{"all", "1.2.3", true},
		{"ALL", "2.0.0", true},
		{"all", "0.0.1", true},
		{"not-a-version", "not-a-version", true},
	}

	for _, tt := range tests {
		if got := Satisfies(tt.declared, tt.disk); got != tt.want {
			t.Errorf("Satisfies(%q, %q) = %v, want %v", tt.declared, tt.disk, got, tt.want)
		}
	}
}

func TestShouldKeep(t *testing.T) {
	disk := []string{"0.9.0", "1.0.0", "1.5.0", "2.0.0"}

	tests := []struct {
		declared, candidate string
		want                bool
	}{
		{"latest", "2.0.0", true},
		{"", "2.0.0", true},
		{"latest", "1.5.0", false},
		{">=1.0.0", "2.0.0", true},
		{">=1.0.0", "1.5.0", false},
		{"<2.0.0", "1.5.0", true},
		{"<2.0.0", "1.0.0", false},
		{"<2.0.0", "2.0.0", false},
		{"1.0.0", "1.0.0", true},
		{"1.0.0", "1.5.0", false},
		{">9.0.0", "2.0.0", false},
		{"all", "0.9.0", true},
		{"all", "1.0.0", true},
		{"all", "2.0.0", true},
	}

	for _, tt := range tests {
		if got := ShouldKeep(tt.declared, disk, tt.candidate); got != tt.want {
			t.Errorf("ShouldKeep(%q, disk, %q) = %v, want %v", tt.declared, tt.candidate, got, tt.want)
		}
	}
}

func TestIsAll(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"all", true},
		{"ALL", true},
		{"All", true},
		{" all ", true},
		{"latest", false},
		{"*", false},
		{"1.2.3", false},
		{"", false},
	}

	for _, tt := range tests {
		if got := IsAll(tt.in); got != tt.want {
			t.Errorf("IsAll(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
