package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/nomo4allas/fact-diagonal/internal/invoice"
)

// Constantes de origen para los registros de Adjuntos.
const (
	tablaRadicado   = "Man_RadicadoFacturas_Test"
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
// y, si existe el registro, actualiza recepción e inserta los adjuntos.
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

	// 1) Actualizar el registro de radicación.
	if err := c.updateRadicado(ctx, rec, fechaCorreo); err != nil {
		c.log.Errorf("    · BD: error actualizando radicado IdDoc=%d: %v", rec.IdDoc, err)
		return err
	}

	// 2) Insertar los adjuntos (PDF y XML).
	for _, a := range adjuntos {
		if len(a.Contenido) == 0 {
			c.log.Infof("    · BD: adjunto %q vacío, se omite su inserción", a.Nombre)
			continue
		}
		if err := c.insertAdjunto(ctx, rec.IdDoc, cufe, a); err != nil {
			c.log.Errorf("    · BD: error insertando adjunto %q: %v", a.Nombre, err)
			return err
		}
	}
	return nil
}

// findByCufe localiza el registro por el campo llave [Cufe/Cude]. Es una
// operación de solo lectura, por lo que se ejecuta también en modo simulación.
//
// Importante: el nombre del campo lleva una barra diagonal; va SIEMPRE entre
// corchetes.
func (c *Client) findByCufe(ctx context.Context, cufe string) (Radicado, bool, error) {
	const query = `
SELECT TOP 1 IdDoc, Nit, NumDocumento, Prefijo
FROM dbo.` + tablaRadicado + `
WHERE [Cufe/Cude] = @cufe`

	var r Radicado
	err := c.dms.QueryRowContext(ctx, query, sql.Named("cufe", cufe)).
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
func (c *Client) updateRadicado(ctx context.Context, rec Radicado, fechaCorreo time.Time) error {
	const query = `
UPDATE dbo.` + tablaRadicado + `
SET ViaDeRecepcion   = @via,
    FechaHoraOriginal = @fecha,
    Usuario          = @usuario,
    Pc               = @pc
WHERE IdDoc = @iddoc`

	// FechaHoraOriginal se almacena en hora de Colombia (UTC-5).
	fechaCol := aHoraColombia(fechaCorreo)

	if c.simulation {
		c.log.Infof("    · BD [SIMULACIÓN] UPDATE %s SET ViaDeRecepcion='%s', FechaHoraOriginal='%s' (hora Colombia UTC-5), Usuario='%s', Pc='%s' WHERE IdDoc=%d",
			tablaRadicado, viaRecepcion, fechaCol.Format("2006-01-02 15:04:05"), usuario, pc, rec.IdDoc)
		return nil
	}

	res, err := c.dms.ExecContext(ctx, query,
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
	c.log.Infof("    · BD: UPDATE radicado IdDoc=%d OK (%d fila(s) afectada(s))", rec.IdDoc, n)
	return nil
}

// insertAdjunto inserta un archivo (PDF o XML) en dbo.Adjuntos.
func (c *Client) insertAdjunto(ctx context.Context, idFuente int64, cufe string, a Adjunto) error {
	const query = `
INSERT INTO dbo.Adjuntos
    (IdFuente, BaseDatosFuente, TablaFuente, NombreAdjunto, Adjunto, Extension, KlFuente)
VALUES
    (@idfuente, @bd, @tabla, @nombre, @adjunto, @ext, @kl)`

	if c.simulation {
		c.log.Infof("    · BD [SIMULACIÓN] INSERT Adjuntos (IdFuente=%d, BaseDatosFuente='%s', TablaFuente='%s', NombreAdjunto='%s', Extension='%s', KlFuente='%s', Adjunto=%d bytes)",
			idFuente, baseDatosFuente, tablaFuente, a.Nombre, a.Extension, cufe, len(a.Contenido))
		return nil
	}

	_, err := c.adj.ExecContext(ctx, query,
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
	c.log.Infof("    · BD: INSERT Adjuntos OK (IdFuente=%d, %s, %d bytes)", idFuente, a.Nombre, len(a.Contenido))
	return nil
}
