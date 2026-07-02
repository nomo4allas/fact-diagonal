// Package database implementa el Módulo 3: integración con SQL Server.
//
// Toda la lógica de negocio (buscar por CUFE, actualizar el radicado e insertar
// los adjuntos) la resuelve el Stored Procedure del cliente
// Spd_IA_DocumentosElectronicos. Este paquete solo lo invoca con las tres
// operaciones definidas (0=buscar, 1=actualizar, 2=insertar adjunto) y traduce
// su @Resultado al desenlace que el pipeline usa para clasificar el correo.
//
// Reglas de seguridad:
//   - Nuestro código NO ejecuta INSERT/UPDATE directos: todo pasa por el SP.
//   - En SIMULATION_MODE no se llama al SP: solo se registran en el log los
//     parámetros que se enviarían en cada operación.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"time"

	_ "github.com/microsoft/go-mssqldb"
)

// Logger es el subconjunto del logger que necesita el paquete.
type Logger interface {
	Infof(format string, args ...any)
	Errorf(format string, args ...any)
}

// Config agrupa los parámetros de conexión a SQL Server.
type Config struct {
	Server   string
	Port     string
	User     string
	Password string
	NameDMS  string // base con Man_RadicadoFacturas_Test
	NameAdj  string // base con la tabla Adjuntos
}

// Client mantiene la conexión a SQL Server y la política de simulación. Todas
// las operaciones se realizan invocando el Stored Procedure del cliente.
type Client struct {
	db         *sql.DB
	log        Logger
	simulation bool
}

// dsn arma la cadena de conexión. Usa encrypt=disable y
// TrustServerCertificate=true, apropiado para el SQL Server local en Docker.
func (c Config) dsn(database string) string {
	port := c.Port
	if port == "" {
		port = "1433"
	}
	q := url.Values{}
	q.Set("database", database)
	q.Set("encrypt", "disable")
	q.Set("TrustServerCertificate", "true")
	q.Set("connection timeout", "30")

	u := &url.URL{
		Scheme:   "sqlserver",
		User:     url.UserPassword(c.User, c.Password),
		Host:     net.JoinHostPort(c.Server, port),
		RawQuery: q.Encode(),
	}
	return u.String()
}

// Open abre la conexión a la base DMS (donde vive el Stored Procedure) y verifica
// la conectividad.
func Open(ctx context.Context, cfg Config, log Logger, simulation bool) (*Client, error) {
	db, err := sql.Open("sqlserver", cfg.dsn(cfg.NameDMS))
	if err != nil {
		return nil, fmt.Errorf("no se pudo preparar la conexión: %w", err)
	}
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)

	c := &Client{db: db, log: log, simulation: simulation}
	if err := c.db.PingContext(ctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("no responde SQL Server: %w", err)
	}
	return c, nil
}

// Close cierra la conexión.
func (c *Client) Close() error {
	if c.db != nil {
		return c.db.Close()
	}
	return nil
}
