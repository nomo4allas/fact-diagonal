package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nomo4allas/fact-diagonal/internal/invoice"
)

// Valores fijos escritos en los registros (no son identificadores de tabla).
const (
	baseDatosFuente = "DMSDiagonal"
	tablaFuente     = "Man_RadicadoFacturas_Test"

	viaRecepcion = "EMAIL"
	usuario      = "AGENTE"
	pc           = "SVR-AGENTE"
)

// bogotaOffset es el desfase horario de Colombia (UTC-5, sin horario de verano).
const bogotaOffset = -5 * time.Hour

// aHoraColombia convierte un instante (las fechas de Graph llegan en UTC) a la
// hora de pared de Colombia (UTC-5). El resultado queda etiquetado como UTC a
// propósito: así el driver de SQL Server escribe esos componentes tal cual en
// columnas sin zona (datetime/datetime2), sin volver a convertir a UTC.
func aHoraColombia(t time.Time) time.Time {
	return t.UTC().Add(bogotaOffset)
}

// tablaRadicado devuelve el nombre calificado de tres partes del radicado.
func (c *Client) tablaRadicado() string {
	return fmt.Sprintf("[%s].dbo.Man_RadicadoFacturas_Test", c.nameDMS)
}

// tablaAdjuntos devuelve el nombre calificado de tres partes de Adjuntos.
func (c *Client) tablaAdjuntos() string {
	return fmt.Sprintf("[%s].dbo.Adjuntos", c.nameAdj)
}

// Radicado es el registro hallado en Man_RadicadoFacturas_Test.
type Radicado struct {
	IdDoc        int64
	Nit          string
	NumDocumento string
	Prefijo      string
}

// Adjunto representa un archivo a insertar en dbo.Adjuntos.
type Adjunto struct {
	Nombre    string // NombreAdjunto
	Extension string // "pdf" | "xml"
	Contenido []byte // Adjunto (varbinary)
}

// PersistInvoice ejecuta el flujo del Módulo 3 para una factura: busca por CUFE
// y, si existe el registro, actualiza recepción e inserta los adjuntos. El
// UPDATE y los INSERT ocurren en una sola transacción (todo o nada).
//
// Casos de borde (siempre se loguean y se continúa sin error):
//   - CUFE vacío           → "sin CUFE, no se procesa BD".
//   - registro no hallado  → "pendiente pre-radicación".
func (c *Client) PersistInvoice(ctx context.Context, data invoice.Data, fechaCorreo time.Time, adjuntos []Adjunto) error {
	cufe := strings.TrimSpace(data.CUFE)
	if cufe == "" {
		c.log.Infof("    · BD: sin CUFE, no se procesa BD")
		return nil
	}

	rec, found, err := c.findByCufe(ctx, cufe)
	if err != nil {
		c.log.Errorf("    · BD: error buscando por [Cufe/Cude]=%s: %v", cufe, err)
		return err
	}
	if !found {
		c.log.Infof("    · BD: pendiente pre-radicación (no existe registro con [Cufe/Cude]=%s)", cufe)
		return nil
	}

	c.log.Infof("    · BD: registro encontrado → IdDoc=%d, Nit=%s, NumDocumento=%s, Prefijo=%s",
		rec.IdDoc, rec.Nit, rec.NumDocumento, rec.Prefijo)

	fechaCol := aHoraColombia(fechaCorreo) // FechaHoraOriginal en hora Colombia (UTC-5)

	if c.simulation {
		c.logSimulacion(rec, cufe, fechaCol, adjuntos)
		return nil
	}
	return c.persistTx(ctx, rec, cufe, fechaCol, adjuntos)
}

// persistTx ejecuta el UPDATE del radicado y los INSERT de adjuntos en una sola
// transacción cross-database. Si algo falla, revierte todo.
func (c *Client) persistTx(ctx context.Context, rec Radicado, cufe string, fechaCol time.Time, adjuntos []Adjunto) (err error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		c.log.Errorf("    · BD: no se pudo iniciar la transacción: %v", err)
		return err
	}
	// Si retornamos con error sin commit, revertimos.
	defer func() {
		if err != nil {
			if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
				c.log.Errorf("    · BD: error en rollback: %v", rbErr)
			} else {
				c.log.Infof("    · BD: transacción revertida (no se escribió nada)")
			}
		}
	}()

	if err = c.updateRadicado(ctx, tx, rec, fechaCol); err != nil {
		c.log.Errorf("    · BD: error actualizando radicado IdDoc=%d: %v", rec.IdDoc, err)
		return err
	}

	for _, a := range adjuntos {
		if len(a.Contenido) == 0 {
			c.log.Infof("    · BD: adjunto %q vacío, se omite su inserción", a.Nombre)
			continue
		}
		if err = c.insertAdjunto(ctx, tx, rec.IdDoc, cufe, a); err != nil {
			c.log.Errorf("    · BD: error insertando adjunto %q: %v", a.Nombre, err)
			return err
		}
	}

	if err = tx.Commit(); err != nil {
		c.log.Errorf("    · BD: error en commit: %v", err)
		return err
	}
	c.log.Infof("    · BD: transacción confirmada (UPDATE + %d adjunto(s)) ✓", len(adjuntos))
	return nil
}

// findByCufe localiza el registro por el campo llave [Cufe/Cude]. Es una
// operación de solo lectura, por lo que se ejecuta también en modo simulación.
//
// Importante: el nombre del campo lleva una barra diagonal; va SIEMPRE entre
// corchetes.
func (c *Client) findByCufe(ctx context.Context, cufe string) (Radicado, bool, error) {
	query := `
SELECT TOP 1 IdDoc, Nit, NumDocumento, Prefijo
FROM ` + c.tablaRadicado() + `
WHERE [Cufe/Cude] = @cufe`

	var r Radicado
	err := c.db.QueryRowContext(ctx, query, sql.Named("cufe", cufe)).
		Scan(&r.IdDoc, &r.Nit, &r.NumDocumento, &r.Prefijo)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Radicado{}, false, nil
	case err != nil:
		return Radicado{}, false, err
	default:
		return r, true, nil
	}
}

// updateRadicado actualiza los campos de recepción del registro hallado.
// NUNCA inserta: solo modifica el registro existente identificado por IdDoc.
func (c *Client) updateRadicado(ctx context.Context, tx *sql.Tx, rec Radicado, fechaCol time.Time) error {
	query := `
UPDATE ` + c.tablaRadicado() + `
SET ViaDeRecepcion    = @via,
    FechaHoraOriginal = @fecha,
    Usuario           = @usuario,
    Pc                = @pc
WHERE IdDoc = @iddoc`

	res, err := tx.ExecContext(ctx, query,
		sql.Named("via", viaRecepcion),
		sql.Named("fecha", fechaCol),
		sql.Named("usuario", usuario),
		sql.Named("pc", pc),
		sql.Named("iddoc", rec.IdDoc),
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	c.log.Infof("    · BD: UPDATE radicado IdDoc=%d (%d fila(s)) → ViaDeRecepcion='%s', FechaHoraOriginal='%s' (UTC-5), Usuario='%s', Pc='%s'",
		rec.IdDoc, n, viaRecepcion, fechaCol.Format("2006-01-02 15:04:05"), usuario, pc)
	return nil
}

// insertAdjunto inserta un archivo (PDF o XML) en dbo.Adjuntos.
func (c *Client) insertAdjunto(ctx context.Context, tx *sql.Tx, idFuente int64, cufe string, a Adjunto) error {
	query := `
INSERT INTO ` + c.tablaAdjuntos() + `
    (IdFuente, BaseDatosFuente, TablaFuente, NombreAdjunto, Adjunto, Extension, KlFuente)
VALUES
    (@idfuente, @bd, @tabla, @nombre, @adjunto, @ext, @kl)`

	_, err := tx.ExecContext(ctx, query,
		sql.Named("idfuente", idFuente),
		sql.Named("bd", baseDatosFuente),
		sql.Named("tabla", tablaFuente),
		sql.Named("nombre", a.Nombre),
		sql.Named("adjunto", a.Contenido),
		sql.Named("ext", a.Extension),
		sql.Named("kl", cufe),
	)
	if err != nil {
		return err
	}
	c.log.Infof("    · BD: INSERT Adjuntos OK (IdFuente=%d, %s '%s', %d bytes)", idFuente, a.Extension, a.Nombre, len(a.Contenido))
	return nil
}

// logSimulacion registra los UPDATE/INSERT que se harían, sin tocar la base.
func (c *Client) logSimulacion(rec Radicado, cufe string, fechaCol time.Time, adjuntos []Adjunto) {
	c.log.Infof("    · BD [SIMULACIÓN] UPDATE %s SET ViaDeRecepcion='%s', FechaHoraOriginal='%s' (hora Colombia UTC-5), Usuario='%s', Pc='%s' WHERE IdDoc=%d",
		tablaFuente, viaRecepcion, fechaCol.Format("2006-01-02 15:04:05"), usuario, pc, rec.IdDoc)
	for _, a := range adjuntos {
		if len(a.Contenido) == 0 {
			c.log.Infof("    · BD [SIMULACIÓN] adjunto %q vacío, se omitiría", a.Nombre)
			continue
		}
		c.log.Infof("    · BD [SIMULACIÓN] INSERT Adjuntos (IdFuente=%d, BaseDatosFuente='%s', TablaFuente='%s', NombreAdjunto='%s', Extension='%s', KlFuente='%s', Adjunto=%d bytes)",
			rec.IdDoc, baseDatosFuente, tablaFuente, a.Nombre, a.Extension, cufe, len(a.Contenido))
	}
}
