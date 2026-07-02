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

// Indicadores de "MASIVA FE": un registro pre-radicado automáticamente por el
// proceso de facturación electrónica de la DIAN (ajuste Módulo 3, req. 2). Solo
// estos registros reciben el flujo completo (recepción + Mandato + Explicacion +
// adjuntos). Los registros que NO cumplen estos cuatro valores se dejan para el
// SP del cliente (nuestro código no los toca).
const (
	ordenadorAutom = "DIANFE"
	usuarioAutom   = "AUTOM"
	pcAutom        = "AUTOM"
	viaAutom       = "MASIVA FE"
)

// EstadoBD resume el desenlace de la persistencia de una factura; el pipeline lo
// usa para decidir la carpeta destino del correo (Procesados/Pendientes/Errores).
type EstadoBD int

const (
	// EstadoNoHallado: no existe registro automático MASIVA FE por CUFE (o el XML
	// no trae CUFE). → Pendientes.
	EstadoNoHallado EstadoBD = iota
	// EstadoPendiente: registro automático hallado pero no se insertó ningún
	// adjunto (0 filas). → Pendientes.
	EstadoPendiente
	// EstadoProcesado: éxito completo (automático con adjuntos insertados).
	// → Procesados.
	EstadoProcesado
)

// Persistencia reporta el resultado de PersistInvoice para clasificar el correo.
type Persistencia struct {
	Estado   EstadoBD
	Adjuntos int // número de adjuntos efectivamente insertados
}

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
	Radicado     string // valor de la columna Radicado; ajuste Módulo 3 req. 1: se usa como IdFuente en Adjuntos (antes se usaba IdDoc)
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

// columnasRadicado son las columnas que leemos del radicado en la búsqueda
// automática por CUFE. Radicado y Nit se castean a varchar para poder escanearlos
// como cadena sea cual sea su tipo subyacente (Nit se almacena como entero en la
// BD; el CAST evita errores de tipo al escanear en un string).
const columnasRadicado = "IdDoc, COALESCE(CAST(Radicado AS varchar(50)), ''), CAST(Nit AS varchar(20)), NumDocumento, Prefijo"

// PersistInvoice ejecuta el flujo del Módulo 3 para una factura y devuelve el
// desenlace (Persistencia) para que el pipeline decida la carpeta destino.
//
// Flujo (ajuste Módulo 3):
//  1. Busca un registro AUTOMÁTICO por CUFE que además cumpla los indicadores
//     MASIVA FE (Ordenador=DIANFE, Usuario=AUTOM, Pc=AUTOM, ViaDeRecepcion=MASIVA FE).
//     Si existe: actualiza recepción + Mandato(Pedido) + Explicacion(DECLARAC) e
//     inserta los adjuntos (IdFuente=Radicado, NotasAdjunto=BL), todo atómico.
//  2. Si no halla ninguno (o el XML no trae CUFE): EstadoNoHallado (pendiente
//     pre-radicación); el registro lo maneja el SP del cliente.
//
// NUNCA escribe [Cufe/Cude] ni hace INSERT en Man_RadicadoFacturas_Test; solo
// UPDATE de recepción sobre registros MASIVA FE existentes hallados por CUFE. En
// SIMULATION_MODE solo lee y registra en el log lo que haría.
func (c *Client) PersistInvoice(ctx context.Context, data invoice.Data, fechaCorreo time.Time, adjuntos []Adjunto) (Persistencia, error) {
	cufe := strings.TrimSpace(data.CUFE)
	if cufe == "" {
		c.log.Infof("    · BD: XML sin CUFE; no se puede ubicar el registro automático → pendiente")
		return Persistencia{Estado: EstadoNoHallado}, nil
	}

	rec, found, err := c.findAuto(ctx, cufe)
	if err != nil {
		c.log.Errorf("    · BD: error buscando registro automático por [Cufe/Cude]=%s: %v", cufe, err)
		return Persistencia{}, err
	}
	if !found {
		c.log.Infof("    · BD: pendiente pre-radicación (no existe registro automático MASIVA FE para esta factura)")
		return Persistencia{Estado: EstadoNoHallado}, nil
	}
	return c.procesarAutomatico(ctx, rec, data, fechaCorreo, adjuntos)
}

// procesarAutomatico aplica el flujo completo a un registro MASIVA FE.
func (c *Client) procesarAutomatico(ctx context.Context, rec Radicado, data invoice.Data, fechaCorreo time.Time, adjuntos []Adjunto) (Persistencia, error) {
	c.log.Infof("    · BD: registro AUTOMÁTICO (MASIVA FE) → IdDoc=%d, Radicado=%s, Nit=%s, NumDocumento=%s, Prefijo=%s",
		rec.IdDoc, rec.Radicado, rec.Nit, rec.NumDocumento, rec.Prefijo)

	fechaCol := aHoraColombia(fechaCorreo) // FechaHoraOriginal en hora Colombia (UTC-5)

	if c.simulation {
		c.logSimulacionAuto(rec, data, fechaCol, adjuntos)
		// En simulación no insertamos, pero reportamos el desenlace que tendría un
		// registro automático con adjuntos presentes, para clasificar la carpeta.
		insertables := contarInsertables(adjuntos)
		estado := EstadoProcesado
		if insertables == 0 {
			estado = EstadoPendiente
		}
		return Persistencia{Estado: estado, Adjuntos: insertables}, nil
	}
	return c.persistAutoTx(ctx, rec, data, fechaCol, adjuntos)
}

// persistAutoTx: UPDATE del radicado (recepción + Mandato + Explicacion) + INSERT
// de adjuntos, todo en una sola transacción cross-database. Si algo falla, revierte.
func (c *Client) persistAutoTx(ctx context.Context, rec Radicado, data invoice.Data, fechaCol time.Time, adjuntos []Adjunto) (_ Persistencia, err error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		c.log.Errorf("    · BD: no se pudo iniciar la transacción: %v", err)
		return Persistencia{}, err
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

	if err = c.updateRadicado(ctx, tx, rec, data, fechaCol); err != nil {
		c.log.Errorf("    · BD: error actualizando radicado IdDoc=%d: %v", rec.IdDoc, err)
		return Persistencia{}, err
	}

	insertados := 0
	for _, a := range adjuntos {
		if len(a.Contenido) == 0 {
			c.log.Infof("    · BD: adjunto %q vacío, se omite su inserción", a.Nombre)
			continue
		}
		if err = c.insertAdjunto(ctx, tx, rec.Radicado, data.BL, a); err != nil {
			c.log.Errorf("    · BD: error insertando adjunto %q: %v", a.Nombre, err)
			return Persistencia{}, err
		}
		insertados++
	}

	if err = tx.Commit(); err != nil {
		c.log.Errorf("    · BD: error en commit: %v", err)
		return Persistencia{}, err
	}
	c.log.Infof("    · BD: transacción confirmada (UPDATE + %d adjunto(s) insertado(s)) ✓", insertados)

	// Ajuste manejo de carpetas: 0 adjuntos insertados ("SP devuelve 0") → Pendientes.
	estado := EstadoProcesado
	if insertados == 0 {
		estado = EstadoPendiente
	}
	return Persistencia{Estado: estado, Adjuntos: insertados}, nil
}

// findAuto localiza un registro AUTOMÁTICO por [Cufe/Cude] que además cumpla los
// indicadores MASIVA FE (ajuste req. 2). Solo lectura: corre también en simulación.
//
// Importante: el nombre del campo [Cufe/Cude] lleva una barra diagonal; va SIEMPRE
// entre corchetes.
func (c *Client) findAuto(ctx context.Context, cufe string) (Radicado, bool, error) {
	query := `
SELECT TOP 1 ` + columnasRadicado + `
FROM ` + c.tablaRadicado() + `
WHERE [Cufe/Cude] = @cufe
  AND Ordenador = @ordenador
  AND Usuario = @usuario
  AND Pc = @pc
  AND ViaDeRecepcion = @via`

	return c.scanRadicado(ctx, query,
		sql.Named("cufe", cufe),
		sql.Named("ordenador", ordenadorAutom),
		sql.Named("usuario", usuarioAutom),
		sql.Named("pc", pcAutom),
		sql.Named("via", viaAutom),
	)
}

// scanRadicado ejecuta una consulta que devuelve las columnasRadicado y mapea la
// única fila esperada (TOP 1). Devuelve found=false ante sql.ErrNoRows.
func (c *Client) scanRadicado(ctx context.Context, query string, args ...any) (Radicado, bool, error) {
	var r Radicado
	err := c.db.QueryRowContext(ctx, query, args...).
		Scan(&r.IdDoc, &r.Radicado, &r.Nit, &r.NumDocumento, &r.Prefijo)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Radicado{}, false, nil
	case err != nil:
		return Radicado{}, false, err
	default:
		return r, true, nil
	}
}

// updateRadicado actualiza los campos de recepción del registro automático hallado
// y, por el ajuste Módulo 3 (req. 3), Mandato=Pedido y Explicacion=DECLARAC.
// NUNCA inserta: solo modifica el registro existente identificado por IdDoc.
//
// Regla (ajuste): Mandato y Explicacion solo se incluyen en el UPDATE cuando el
// valor extraído NO viene vacío. Si viene vacío, se omiten del SET para no pisar
// un valor que ya pudiera existir en la BD.
func (c *Client) updateRadicado(ctx context.Context, tx *sql.Tx, rec Radicado, data invoice.Data, fechaCol time.Time) error {
	sets, args := setsUpdateRadicado(data, fechaCol, rec.IdDoc)
	query := `
UPDATE ` + c.tablaRadicado() + `
SET ` + strings.Join(sets, ",\n    ") + `
WHERE IdDoc = @iddoc`

	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	c.log.Infof("    · BD: UPDATE radicado IdDoc=%d (%d fila(s)) → ViaDeRecepcion='%s', FechaHoraOriginal='%s' (UTC-5), Usuario='%s', Pc='%s', Mandato=%s, Explicacion=%s",
		rec.IdDoc, n, viaRecepcion, fechaCol.Format("2006-01-02 15:04:05"), usuario, pc, campoOpcionalLog(data.Pedido), campoOpcionalLog(data.Declarac))
	return nil
}

// setsUpdateRadicado arma las asignaciones y los argumentos del UPDATE del
// radicado. Los campos de recepción (ViaDeRecepcion, FechaHoraOriginal, Usuario,
// Pc) van siempre; Mandato (Pedido) y Explicacion (DECLARAC) solo se agregan si
// el valor extraído no viene vacío, para no sobrescribir lo que ya haya en la BD.
func setsUpdateRadicado(data invoice.Data, fechaCol time.Time, iddoc int64) (sets []string, args []any) {
	sets = []string{
		"ViaDeRecepcion    = @via",
		"FechaHoraOriginal = @fecha",
		"Usuario           = @usuario",
		"Pc                = @pc",
	}
	args = []any{
		sql.Named("via", viaRecepcion),
		sql.Named("fecha", fechaCol),
		sql.Named("usuario", usuario),
		sql.Named("pc", pc),
		sql.Named("iddoc", iddoc),
	}
	if mandato := strings.TrimSpace(data.Pedido); mandato != "" { // ajuste req. 3
		sets = append(sets, "Mandato = @mandato")
		args = append(args, sql.Named("mandato", mandato))
	}
	if explicacion := strings.TrimSpace(data.Declarac); explicacion != "" { // ajuste req. 3
		sets = append(sets, "Explicacion = @explicacion")
		args = append(args, sql.Named("explicacion", explicacion))
	}
	return sets, args
}

// campoOpcionalLog formatea Mandato/Explicacion para el log: el valor entre
// comillas si se actualiza, o "(omitido: vacío)" si se dejó fuera del UPDATE por
// venir vacío (regla: no pisar lo que ya exista en la BD).
func campoOpcionalLog(v string) string {
	if strings.TrimSpace(v) == "" {
		return "(omitido: vacío)"
	}
	return "'" + strings.TrimSpace(v) + "'"
}

// insertAdjunto inserta un archivo (PDF o XML) en dbo.Adjuntos.
//
// Ajustes Módulo 3:
//   - req. 1: IdFuente = campo Radicado (antes IdDoc).
//   - req. 5: NotasAdjunto = BL extraído del PDF (NULL si viene vacío).
//   - KlFuente va SIEMPRE NULL para facturas electrónicas: el CUFE no se guarda
//     en este campo (antes se escribía; se retiró por requerimiento del cliente).
func (c *Client) insertAdjunto(ctx context.Context, tx *sql.Tx, idFuente, bl string, a Adjunto) error {
	query := `
INSERT INTO ` + c.tablaAdjuntos() + `
    (IdFuente, BaseDatosFuente, TablaFuente, NombreAdjunto, Adjunto, Extension, KlFuente, NotasAdjunto)
VALUES
    (@idfuente, @bd, @tabla, @nombre, @adjunto, @ext, NULL, @notas)`

	_, err := tx.ExecContext(ctx, query,
		sql.Named("idfuente", idFuente), // ajuste req. 1: valor de la columna Radicado
		sql.Named("bd", baseDatosFuente),
		sql.Named("tabla", tablaFuente),
		sql.Named("nombre", a.Nombre),
		sql.Named("adjunto", a.Contenido),
		sql.Named("ext", a.Extension),
		sql.Named("notas", notasAdjuntoSQL(bl)), // ajuste req. 5: BL (NULL si viene vacío)
	)
	if err != nil {
		return err
	}
	c.log.Infof("    · BD: INSERT Adjuntos OK (IdFuente=%s, %s '%s', %d bytes, KlFuente=NULL, NotasAdjunto=%s)",
		idFuente, a.Extension, a.Nombre, len(a.Contenido), notasAdjuntoLog(bl))
	return nil
}

// notasAdjuntoSQL devuelve el valor a escribir en NotasAdjunto: el BL (sin
// espacios) si tiene valor, o nil (→ NULL en SQL) si viene vacío, para no dejar
// cadenas vacías en la columna.
func notasAdjuntoSQL(bl string) any {
	if s := strings.TrimSpace(bl); s != "" {
		return s
	}
	return nil
}

// notasAdjuntoLog formatea NotasAdjunto para el log: 'valor' si se escribe el BL,
// o NULL si viene vacío.
func notasAdjuntoLog(bl string) string {
	if s := strings.TrimSpace(bl); s != "" {
		return "'" + s + "'"
	}
	return "NULL"
}

// logSimulacionAuto registra el UPDATE/INSERT que se harían para un registro
// automático, sin tocar la base.
func (c *Client) logSimulacionAuto(rec Radicado, data invoice.Data, fechaCol time.Time, adjuntos []Adjunto) {
	c.log.Infof("    · BD [SIMULACIÓN] UPDATE %s SET ViaDeRecepcion='%s', FechaHoraOriginal='%s' (hora Colombia UTC-5), Usuario='%s', Pc='%s', Mandato=%s, Explicacion=%s WHERE IdDoc=%d",
		tablaFuente, viaRecepcion, fechaCol.Format("2006-01-02 15:04:05"), usuario, pc, campoOpcionalLog(data.Pedido), campoOpcionalLog(data.Declarac), rec.IdDoc)
	for _, a := range adjuntos {
		if len(a.Contenido) == 0 {
			c.log.Infof("    · BD [SIMULACIÓN] adjunto %q vacío, se omitiría", a.Nombre)
			continue
		}
		c.log.Infof("    · BD [SIMULACIÓN] INSERT Adjuntos (IdFuente=%s, BaseDatosFuente='%s', TablaFuente='%s', NombreAdjunto='%s', Extension='%s', KlFuente=NULL, NotasAdjunto=%s, Adjunto=%d bytes)",
			rec.Radicado, baseDatosFuente, tablaFuente, a.Nombre, a.Extension, notasAdjuntoLog(data.BL), len(a.Contenido))
	}
}

// contarInsertables cuenta los adjuntos con contenido no vacío (los que se
// insertarían realmente).
func contarInsertables(adjuntos []Adjunto) int {
	n := 0
	for _, a := range adjuntos {
		if len(a.Contenido) > 0 {
			n++
		}
	}
	return n
}
