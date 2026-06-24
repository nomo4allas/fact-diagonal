// Package database implementa el Módulo 3: integración con SQL Server.
//
// Por cada factura extraída por el Módulo 2 busca su registro en
// Man_RadicadoFacturas_Test usando el campo llave [Cufe/Cude] (cuyo nombre
// lleva una barra diagonal y por eso SIEMPRE se escribe entre corchetes). Si lo
// encuentra, actualiza los campos de recepción e inserta el PDF y el XML en la
// tabla dbo.Adjuntos.
//
// Reglas de seguridad:
//   - NUNCA hace INSERT en Man_RadicadoFacturas_Test; solo UPDATE de registros
//     existentes hallados por CUFE.
//   - En SIMULATION_MODE solo ejecuta lecturas (la búsqueda por CUFE) y registra
//     en el log los UPDATE/INSERT que haría, sin tocar la base.
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
	Server    string
	Port      string
	User      string
	Password  string
	NameDMS   string // base con Man_RadicadoFacturas_Test
	NameAdj   string // base con la tabla Adjuntos
}

// Client mantiene las conexiones a las dos bases (DMS y Adjuntos) y la política
// de simulación.
type Client struct {
	dms        *sql.DB
	adj        *sql.DB
	log        Logger
	simulation bool
}

// dsn arma la cadena de conexión para una base concreta. Usa encrypt=disable y
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

// Open abre las conexiones a ambas bases y verifica la conectividad.
func Open(ctx context.Context, cfg Config, log Logger, simulation bool) (*Client, error) {
	dms, err := openDB(cfg.dsn(cfg.NameDMS))
	if err != nil {
		return nil, fmt.Errorf("no se pudo preparar la conexión a %s: %w", cfg.NameDMS, err)
	}
	adj, err := openDB(cfg.dsn(cfg.NameAdj))
	if err != nil {
		dms.Close()
		return nil, fmt.Errorf("no se pudo preparar la conexión a %s: %w", cfg.NameAdj, err)
	}

	c := &Client{dms: dms, adj: adj, log: log, simulation: simulation}
	if err := c.ping(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func openDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		return nil, err
	}
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	return db, nil
}

// ping comprueba que ambas bases respondan.
func (c *Client) ping(ctx context.Context) error {
	if err := c.dms.PingContext(ctx); err != nil {
		return fmt.Errorf("no responde la base DMS: %w", err)
	}
	if err := c.adj.PingContext(ctx); err != nil {
		return fmt.Errorf("no responde la base Adjuntos: %w", err)
	}
	return nil
}

// Close cierra ambas conexiones.
func (c *Client) Close() error {
	var firstErr error
	if c.dms != nil {
		if err := c.dms.Close(); err != nil {
			firstErr = err
		}
	}
	if c.adj != nil {
		if err := c.adj.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
