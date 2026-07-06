package license

import (
	"os"
	"strings"
	"testing"

	"github.com/nomo4allas/fact-diagonal/internal/config"
)

// TestGenerateDeterministic verifica que Generate sea determinista y sensible a
// sus entradas: misma entrada → misma clave; distinta entrada → distinta clave.
func TestGenerateDeterministic(t *testing.T) {
	k1 := Generate("DIAGONAL", "DIAGAPP01")
	k2 := Generate("DIAGONAL", "DIAGAPP01")
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
	if k1 == Generate("OTRO", "DIAGAPP01") {
		t.Error("clave no cambió al cambiar el cliente")
	}
	if k1 == Generate("DIAGONAL", "OTROHOST") {
		t.Error("clave no cambió al cambiar el hostname")
	}
}

// TestValidateRoundTrip comprueba el ciclo completo: una clave emitida por
// Generate para el hostname real de la máquina es aceptada por Validate, y
// cualquier alteración de la clave o del hostname la rechaza.
func TestValidateRoundTrip(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("no se pudo obtener el hostname: %v", err)
	}

	const client = "DIAGONAL"

	// Config válida: ALLOWED_HOST = hostname real, LICENSE_KEY = clave emitida.
	valid := &config.Config{
		AllowedHost: host,
		ClientName:  client,
		LicenseKey:  Generate(client, host),
	}
	if err := Validate(valid); err != nil {
		t.Fatalf("Validate rechazó una licencia válida: %v", err)
	}

	// La comparación de la clave es case-insensitive (Generate produce minúsculas).
	upper := &config.Config{
		AllowedHost: host,
		ClientName:  client,
		LicenseKey:  strings.ToUpper(valid.LicenseKey),
	}
	if err := Validate(upper); err != nil {
		t.Errorf("Validate rechazó una clave válida en mayúsculas: %v", err)
	}

	// Nivel 2 roto: clave incorrecta con hostname correcto.
	badKey := &config.Config{
		AllowedHost: host,
		ClientName:  client,
		LicenseKey:  Generate(client, host+"x"),
	}
	if err := Validate(badKey); err == nil {
		t.Error("Validate aceptó una clave inválida")
	}

	// Nivel 2 roto: clave vacía.
	empty := &config.Config{
		AllowedHost: host,
		ClientName:  client,
		LicenseKey:  "",
	}
	if err := Validate(empty); err == nil {
		t.Error("Validate aceptó una LICENSE_KEY vacía")
	}

	// Nivel 1 roto: ALLOWED_HOST no coincide con el hostname real. Aunque la clave
	// corresponda a ese host distinto, debe rechazarse.
	wrongHost := &config.Config{
		AllowedHost: host + "-NOPE",
		ClientName:  client,
		LicenseKey:  Generate(client, host+"-NOPE"),
	}
	if err := Validate(wrongHost); err == nil {
		t.Error("Validate aceptó un hostname no autorizado")
	}
}
