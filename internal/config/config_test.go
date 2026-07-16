package config

import (
	"os"
	"path/filepath"
	"testing"
)

// escribirConfig deja un config.env mínimo y válido en un directorio temporal,
// con las líneas extra indicadas, y devuelve su ruta.
func escribirConfig(t *testing.T, extra string) string {
	t.Helper()
	contenido := "TENANT_ID=t\nCLIENT_ID=c\nCLIENT_SECRET=s\nMAILBOX=buzon@empresa.com\n" + extra
	ruta := filepath.Join(t.TempDir(), "config.env")
	if err := os.WriteFile(ruta, []byte(contenido), 0o600); err != nil {
		t.Fatalf("no se pudo escribir el config de prueba: %v", err)
	}
	return ruta
}

// limpiarEntorno aísla el test de las variables ya presentes en el proceso:
// godotenv NO sobrescribe las que existen, así que una INBOX_FOLDER heredada
// falsearía el resultado.
func limpiarEntorno(t *testing.T, claves ...string) {
	t.Helper()
	for _, k := range claves {
		if v, ok := os.LookupEnv(k); ok {
			t.Cleanup(func() { os.Setenv(k, v) })
			os.Unsetenv(k)
		}
	}
}

// TestInboxFolderPorDefecto: sin INBOX_FOLDER el campo queda vacío, que es lo que
// main.go interpreta como "leer de la Bandeja de entrada" (comportamiento actual).
func TestInboxFolderPorDefecto(t *testing.T) {
	limpiarEntorno(t, "INBOX_FOLDER")

	cfg, err := Load(escribirConfig(t, ""))
	if err != nil {
		t.Fatalf("Load devolvió error: %v", err)
	}
	if cfg.InboxFolder != "" {
		t.Errorf("InboxFolder = %q, se esperaba vacío (→ Inbox)", cfg.InboxFolder)
	}
}

// TestInboxFolderDefinida: con INBOX_FOLDER=Pruebas el valor llega a la Config.
func TestInboxFolderDefinida(t *testing.T) {
	limpiarEntorno(t, "INBOX_FOLDER")

	cfg, err := Load(escribirConfig(t, "INBOX_FOLDER=Pruebas\n"))
	if err != nil {
		t.Fatalf("Load devolvió error: %v", err)
	}
	if cfg.InboxFolder != "Pruebas" {
		t.Errorf("InboxFolder = %q, want %q", cfg.InboxFolder, "Pruebas")
	}
}

// TestInboxFolderVacíaOConEspacios: una variable presente pero vacía (o solo con
// espacios) equivale a no definirla → Bandeja de entrada.
func TestInboxFolderVaciaOConEspacios(t *testing.T) {
	casos := []struct{ nombre, linea string }{
		{"vacía", "INBOX_FOLDER=\n"},
		{"solo espacios", "INBOX_FOLDER=   \n"},
	}
	for _, k := range casos {
		t.Run(k.nombre, func(t *testing.T) {
			limpiarEntorno(t, "INBOX_FOLDER")

			cfg, err := Load(escribirConfig(t, k.linea))
			if err != nil {
				t.Fatalf("Load devolvió error: %v", err)
			}
			if cfg.InboxFolder != "" {
				t.Errorf("InboxFolder = %q, se esperaba vacío (→ Inbox)", cfg.InboxFolder)
			}
		})
	}
}

// TestInboxFolderSeRecorta: los espacios alrededor del nombre no deben viajar al
// displayName con el que se busca la carpeta en Graph.
func TestInboxFolderSeRecorta(t *testing.T) {
	limpiarEntorno(t, "INBOX_FOLDER")

	cfg, err := Load(escribirConfig(t, "INBOX_FOLDER=  Pruebas  \n"))
	if err != nil {
		t.Fatalf("Load devolvió error: %v", err)
	}
	if cfg.InboxFolder != "Pruebas" {
		t.Errorf("InboxFolder = %q, want %q (sin espacios)", cfg.InboxFolder, "Pruebas")
	}
}
