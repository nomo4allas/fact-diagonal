// Package database implementa el Módulo 3: integración con SQL Server.
//
// Por cada factura extraída por el Módulo 2 busca su registro en
// Man_RadicadoFacturas_Test usando el campo llave [Cufe/Cude] (cuyo nombre
// lleva una barra diagonal y por eso SIEMPRE se escribe entre corchetes). Si lo
// encuentra, actualiza los campos de recepción e inserta el PDF y el XML en la
// tabla dbo.Adjuntos, todo dentro de una misma transacción.
//
// Como el radicado vive en una base (DMSDiagonal) y los adjuntos en otra
// (Adjuntos), se usa una ÚNICA conexión y nombres calificados de tres partes
// ([base].dbo.tabla). SQL Server resuelve la transacción cross-database dentro
// de la misma instancia sin necesidad de MSDTC.
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
	Server   string
	Port     string
	User     string
	Password string
	NameDMS  string // base con Man_RadicadoFacturas_Test
	NameAdj  string // base con la tabla Adjuntos
}

// Client mantiene la conexión a SQL Server y la política de simulación. Usa una
// sola conexión para poder ejecutar transacciones que abarcan ambas bases.
type Client struct {
	db         *sql.DB
	nameDMS    string
	nameAdj    string
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

// Open abre la conexión (a la base DMS, desde la que se referencian ambas bases
// por nombre calificado) y verifica la conectividad.
func Open(ctx context.Context, cfg Config, log Logger, simulation bool) (*Client, error) {
	db, err := sql.Open("sqlserver", cfg.dsn(cfg.NameDMS))
	if err != nil {
		return nil, fmt.Errorf("no se pudo preparar la conexión: %w", err)
	}
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)

	c := &Client{db: db, nameDMS: cfg.NameDMS, nameAdj: cfg.NameAdj, log: log, simulation: simulation}
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
