package storagemedium

import "testing"

// TestResolveRoot vérifie que la racine résout vers un support connu (device physique
// réel). Sur la machine de campagne, la racine vit sur un NVMe → "ssd" ; l'invariant dur
// est qu'un chemin adossé à un vrai device bloc n'est jamais "unknown".
func TestResolveRoot(t *testing.T) {
	got := Resolve("/")
	if got.Medium == Unknown {
		t.Fatalf("Resolve(\"/\") = unknown ; la racine devrait résoudre un device bloc (device=%q)", got.Device)
	}
	if got.Medium != SSD && got.Medium != Rotational {
		t.Fatalf("Resolve(\"/\").Medium = %q ; attendu ssd ou rotational", got.Medium)
	}
	if got.Device == "" {
		t.Fatalf("Resolve(\"/\").Device vide alors que Medium=%q", got.Medium)
	}
	// La machine de campagne est un NVMe (SSD). Signalé, non bloquant sur d'autres cibles.
	if got.Medium != SSD {
		t.Logf("racine résolue en %q (device=%q) ; machine de campagne attendue en ssd", got.Medium, got.Device)
	}
}

// TestResolveNonexistent vérifie le fail-soft : un chemin inexistant rend "unknown"
// sans erreur ni panique, jamais un support inventé.
func TestResolveNonexistent(t *testing.T) {
	got := Resolve("/chemin/qui/nexiste/pas/vraiment/12345")
	if got.Medium != Unknown {
		t.Fatalf("Resolve(chemin absent) = %q ; attendu unknown (fail-soft)", got.Medium)
	}
	if got.Device != "" {
		t.Fatalf("Resolve(chemin absent).Device = %q ; attendu vide", got.Device)
	}
}

// TestClassify couvre l'interprétation de queue/rotational.
func TestClassify(t *testing.T) {
	cases := map[string]string{"1\n": Rotational, "0\n": SSD, "": Unknown, "2": Unknown}
	for in, want := range cases {
		if got := classify([]byte(in)); got != want {
			t.Errorf("classify(%q) = %q ; attendu %q", in, got, want)
		}
	}
}

// TestMajorMinor vérifie le décodage dev_t glibc sur une valeur connue (nvme0n1p2 =
// 259:5, dev_t 66309) mesurée au sol sur la machine de campagne.
func TestMajorMinor(t *testing.T) {
	const dev = 66309
	if maj := major(dev); maj != 259 {
		t.Errorf("major(66309) = %d ; attendu 259", maj)
	}
	if min := minor(dev); min != 5 {
		t.Errorf("minor(66309) = %d ; attendu 5", min)
	}
}
