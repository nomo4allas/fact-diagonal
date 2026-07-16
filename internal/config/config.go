// Package config carga y valida la configuración de la aplicación
// a partir del archivo config.env.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Config contiene las credenciales y parámetros operativos del agente.
type Config struct {
	TenantID       string // ID del tenant de Azure AD (Directory ID)
	ClientID       string // ID de la aplicación registrada (App registration)
	ClientSecret   string // Secreto del cliente
	Mailbox        string // Buzón a leer, p.ej. facturae@diagonal.com.co
	InboxFolder    string // Opcional: carpeta de entrada (displayName) a leer en lugar de la Bandeja de entrada. Vacía → Inbox.
	SimulationMode bool   // Si true, el agente solo lee y nunca modifica nada
	GeminiAPIKey   string // Clave de Gemini API (opcional, último eslabón de la cascada)
	GeminiModel    string // Modelo de Gemini a usar (por defecto gemini-2.0-flash)
	GeminiLocation string // Región/location de Gemini (por defecto global)
	FilterFrom     string // Opcional: si no está vacío, solo procesa correos cuyo remitente lo contenga (subcadena, case-insensitive)
	MaxCorreos     int    // Máximo de correos a procesar por corrida (por defecto 5 si no se define)
	SMTPTo         string // Destinatario de las notificaciones de error (p.ej. soporte@diagonal.com.co). El remitente es Mailbox.

	// Licenciamiento. Ver internal/license.
	AllowedHost string // OBSOLETO: se ignora en la validación (se conserva por compatibilidad)
	ClientName  string // Nombre del cliente al que se licencia (única entrada del HMAC)
	LicenseKey  string // Clave de licencia HMAC-SHA256 en hex

	// Módulo 3 — SQL Server. Opcionales: si DBServer está vacío, el módulo se omite.
	DBServer   string // host del servidor SQL Server
	DBPort     string // puerto (p.ej. 1433)
	DBUser     string // usuario
	DBPassword string // contraseña
	DBNameDMS  string // base de datos con Man_RadicadoFacturas_Test (p.ej. DMSDiagonal)
	DBNameAdj  string // base de datos con la tabla Adjuntos
	SPName     string // nombre del Stored Procedure a invocar (por defecto Spd_IA_DocumentosElectronicos)
}

// DBEnabled indica si hay configuración suficiente para activar el Módulo 3.
func (c *Config) DBEnabled() bool {
	return c.DBServer != "" && c.DBUser != "" && c.DBNameDMS != "" && c.DBNameAdj != ""
}

// Load lee el archivo indicado (por defecto "config.env"), valida los
// campos obligatorios y devuelve una Config lista para usar.
func Load(path string) (*Config, error) {
	if path == "" {
		path = "config.env"
	}

	// godotenv.Load no sobrescribe variables ya presentes en el entorno,
	// lo que permite inyectar valores en despliegues sin tocar el archivo.
	if err := godotenv.Load(path); err != nil {
		return nil, fmt.Errorf("no se pudo cargar %s: %w", path, err)
	}

	cfg := &Config{
		TenantID:       strings.TrimSpace(os.Getenv("TENANT_ID")),
		ClientID:       strings.TrimSpace(os.Getenv("CLIENT_ID")),
		ClientSecret:   strings.TrimSpace(os.Getenv("CLIENT_SECRET")),
		Mailbox:        strings.TrimSpace(os.Getenv("MAILBOX")),
		InboxFolder:    strings.TrimSpace(os.Getenv("INBOX_FOLDER")),
		SimulationMode: parseBool(os.Getenv("SIMULATION_MODE"), true),
		GeminiAPIKey:   strings.TrimSpace(os.Getenv("GEMINI_API_KEY")),
		GeminiModel:    orDefault(os.Getenv("GEMINI_MODEL"), "gemini-2.0-flash"),
		GeminiLocation: orDefault(os.Getenv("GEMINI_LOCATION"), "global"),
		FilterFrom:     strings.TrimSpace(os.Getenv("FILTER_FROM")),
		MaxCorreos:     parseInt(os.Getenv("MAX_CORREOS"), 5),
		SMTPTo:         strings.TrimSpace(os.Getenv("SMTP_TO")),
		AllowedHost:    strings.TrimSpace(os.Getenv("ALLOWED_HOST")),
		ClientName:     strings.TrimSpace(os.Getenv("CLIENT_NAME")),
		LicenseKey:     strings.TrimSpace(os.Getenv("LICENSE_KEY")),
		DBServer:       strings.TrimSpace(os.Getenv("DB_SERVER")),
		DBPort:         strings.TrimSpace(os.Getenv("DB_PORT")),
		DBUser:         strings.TrimSpace(os.Getenv("DB_USER")),
		DBPassword:     os.Getenv("DB_PASSWORD"), // sin TrimSpace: la contraseña puede tener espacios significativos
		DBNameDMS:      strings.TrimSpace(os.Getenv("DB_NAME_DMS")),
		DBNameAdj:      strings.TrimSpace(os.Getenv("DB_NAME_ADJ")),
		SPName:         orDefault(os.Getenv("SP_NAME"), "Spd_IA_DocumentosElectronicos"),
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// validate comprueba que los campos obligatorios estén presentes.
func (c *Config) validate() error {
	missing := make([]string, 0, 4)
	if c.TenantID == "" {
		missing = append(missing, "TENANT_ID")
	}
	if c.ClientID == "" {
		missing = append(missing, "CLIENT_ID")
	}
	if c.ClientSecret == "" {
		missing = append(missing, "CLIENT_SECRET")
	}
	if c.Mailbox == "" {
		missing = append(missing, "MAILBOX")
	}
	if len(missing) > 0 {
		return fmt.Errorf("faltan variables obligatorias en config.env: %s", strings.Join(missing, ", "))
	}
	return nil
}

// parseBool interpreta valores comunes de verdadero/falso; ante un valor
// vacío o no reconocido devuelve el valor por defecto indicado.
func parseBool(v string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "si", "sí", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		return def
	}
}

// orDefault devuelve v sin espacios, o def si v viene vacío.
func orDefault(v, def string) string {
	if s := strings.TrimSpace(v); s != "" {
		return s
	}
	return def
}

// parseInt interpreta un entero positivo; ante un valor vacío, no numérico o
// menor o igual a cero devuelve el valor por defecto indicado.
func parseInt(v string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return def
	}
	return n
}
