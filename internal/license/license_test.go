package license

import (
	"strings"
	"testing"

	"github.com/nomo4allas/fact-diagonal/internal/config"
)

// TestGenerateDeterministic verifica que Generate sea determinista y sensible a
// su entrada: mismo cliente → misma clave; distinto cliente → distinta clave.
func TestGenerateDeterministic(t *testing.T) {
	k1 := Generate("DIAGONAL")
	k2 := Generate("DIAGONAL")
	if k1 != k2 {
		t.Fatalf("Generate no es determinista: %q != %q", k1, k2)
	}
	if k1 == "" {
		t.Fatal("Generate devolvió cadena vacía")
	}
	// HMAC-SHA256 en hex → 64 caracteres.
	if len(k1) != 64 {
		t.Fatalf("longitud inesperada de la clave: %d (se esperaba 64)", len(k1))
	}
	if k1 == Generate("OTRO") {
		t.Error("clave no cambió al cambiar el cliente")
	}
}

// TestValidateRoundTrip comprueba el ciclo completo: una clave emitida por
// Generate para el cliente es aceptada por Validate, y cualquier alteración de
// la clave la rechaza.
func TestValidateRoundTrip(t *testing.T) {
	const client = "DIAGONAL"

	// Config válida: LICENSE_KEY = clave emitida para CLIENT_NAME.
	valid := &config.Config{
		ClientName: client,
		LicenseKey: Generate(client),
	}
	if err := Validate(valid); err != nil {
		t.Fatalf("Validate rechazó una licencia válida: %v", err)
	}

	// La comparación de la clave es case-insensitive (Generate produce minúsculas).
	upper := &config.Config{
		ClientName: client,
		LicenseKey: strings.ToUpper(valid.LicenseKey),
	}
	if err := Validate(upper); err != nil {
		t.Errorf("Validate rechazó una clave válida en mayúsculas: %v", err)
	}

	// ALLOWED_HOST se ignora: una config con un host cualquiera pero clave válida
	// debe seguir aceptándose.
	ignoredHost := &config.Config{
		AllowedHost: "CUALQUIER-HOST",
		ClientName:  client,
		LicenseKey:  Generate(client),
	}
	if err := Validate(ignoredHost); err != nil {
		t.Errorf("Validate rechazó una licencia válida por causa de ALLOWED_HOST: %v", err)
	}

	// Clave incorrecta: derivada de otro cliente.
	badKey := &config.Config{
		ClientName: client,
		LicenseKey: Generate(client + "x"),
	}
	if err := Validate(badKey); err == nil {
		t.Error("Validate aceptó una clave inválida")
	}

	// Clave vacía.
	empty := &config.Config{
		ClientName: client,
		LicenseKey: "",
	}
	if err := Validate(empty); err == nil {
		t.Error("Validate aceptó una LICENSE_KEY vacía")
	}
}
