package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
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
// adjuntos). Los registros que NO cumplen estos cuatro valores se consideran
// MANUALES y reciben un tratamiento distinto (ver findManual / updateCufe).
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
	// EstadoNoHallado: no existe registro (ni automático por CUFE ni manual por
	// NumDocumento+Prefijo+NIT). → Pendientes.
	EstadoNoHallado EstadoBD = iota
	// EstadoPendiente: registro automático hallado pero no se insertó ningún
	// adjunto (0 filas). → Pendientes.
	EstadoPendiente
	// EstadoProcesado: éxito completo (automático con adjuntos insertados, o
	// manual con su CUFE actualizado). → Procesados.
	EstadoProcesado
)

// Persistencia reporta el resultado de PersistInvoice para clasificar el correo.
type Persistencia struct {
	Estado   EstadoBD
	Manual   bool // el registro hallado era manual (sin indicadores MASIVA FE)
	Adjuntos int  // número de adjuntos efectivamente insertados
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
	Manual       bool // true si el registro NO tiene los indicadores MASIVA FE
}

// Adjunto representa un archivo a insertar en dbo.Adjuntos.
type Adjunto struct {
	Nombre    string // NombreAdjunto
	Extension string // "pdf" | "xml"
	Contenido []byte // Adjunto (varbinary)
}

// columnasRadicado son las columnas que leemos del radicado, compartidas por las
// dos búsquedas (automática y manual). Radicado y Nit se castean a varchar para
// poder escanearlos como cadena sea cual sea su tipo subyacente (Nit se almacena
// como entero en la BD; el CAST evita errores de tipo al escanear en un string).
const columnasRadicado = "IdDoc, COALESCE(CAST(Radicado AS varchar(50)), ''), CAST(Nit AS varchar(20)), NumDocumento, Prefijo"

// PersistInvoice ejecuta el flujo del Módulo 3 para una factura y devuelve el
// desenlace (Persistencia) para que el pipeline decida la carpeta destino.
//
// Flujo (ajuste Módulo 3):
//  1. Busca un registro AUTOMÁTICO por CUFE que además cumpla los indicadores
//     MASIVA FE (Ordenador=DIANFE, Usuario=AUTOM, Pc=AUTOM, ViaDeRecepcion=MASIVA FE).
//     Si existe: actualiza recepción + Mandato(Pedido) + Explicacion(DECLARAC) e
//     inserta los adjuntos (IdFuente=Radicado, NotasAdjunto=BL), todo atómico.
//  2. Si no hay automático, busca un registro MANUAL por NumDocumento+Prefijo+NIT
//     (su [Cufe/Cude] suele venir vacío) y le escribe el CUFE extraído del XML.
//  3. Si no halla ninguno: EstadoNoHallado (pendiente pre-radicación).
//
// NUNCA hace INSERT en Man_RadicadoFacturas_Test; solo UPDATE de registros
// existentes. En SIMULATION_MODE solo lee y registra en el log lo que haría.
func (c *Client) PersistInvoice(ctx context.Context, data invoice.Data, fechaCorreo time.Time, adjuntos []Adjunto) (Persistencia, error) {
	cufe := strings.TrimSpace(data.CUFE)

	// 1) Intento AUTOMÁTICO por CUFE + indicadores MASIVA FE.
	if cufe != "" {
		rec, found, err := c.findAuto(ctx, cufe)
		if err != nil {
			c.log.Errorf("    · BD: error buscando registro automático por [Cufe/Cude]=%s: %v", cufe, err)
			return Persistencia{}, err
		}
		if found {
			return c.procesarAutomatico(ctx, rec, data, fechaCorreo, adjuntos)
		}
	} else {
		c.log.Infof("    · BD: XML sin CUFE; se omite la búsqueda automática y se intenta match manual")
	}

	// 2) Intento MANUAL por NumDocumento+Prefijo+NIT.
	numDoc, prefijo := splitNumero(data.Numero, data.Prefijo)
	nit, okNit := normalizeNITNumeric(data.NIT) // el XML trae puntos/DV; la BD guarda el entero base
	if numDoc == "" || !okNit {
		c.log.Infof("    · BD: sin datos suficientes para match manual (NumDocumento=%q, NIT=%q no numérico) → pendiente", numDoc, data.NIT)
		return Persistencia{Estado: EstadoNoHallado}, nil
	}
	rec, found, err := c.findManual(ctx, numDoc, prefijo, nit)
	if err != nil {
		c.log.Errorf("    · BD: error buscando registro manual (Num=%s, Prefijo=%q, Nit=%d): %v", numDoc, prefijo, nit, err)
		return Persistencia{}, err
	}
	if !found {
		c.log.Infof("    · BD: pendiente pre-radicación (no existe registro automático ni manual para esta factura)")
		return Persistencia{Estado: EstadoNoHallado}, nil
	}
	return c.procesarManual(ctx, rec, cufe)
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

// procesarManual escribe el CUFE del XML en un registro manual (ajuste req. 4).
func (c *Client) procesarManual(ctx context.Context, rec Radicado, cufe string) (Persistencia, error) {
	c.log.Infof("    · BD: registro MANUAL → IdDoc=%d, Radicado=%s, Nit=%s, NumDocumento=%s, Prefijo=%s",
		rec.IdDoc, rec.Radicado, rec.Nit, rec.NumDocumento, rec.Prefijo)

	if cufe == "" {
		// Registro manual hallado pero el XML no trae CUFE: no hay nada que escribir.
		c.log.Infof("    · BD: registro manual hallado pero el XML no trae CUFE; no se actualiza → pendiente")
		return Persistencia{Estado: EstadoPendiente, Manual: true}, nil
	}

	if c.simulation {
		c.log.Infof("    · BD [SIMULACIÓN] UPDATE %s SET [Cufe/Cude]='%s' WHERE IdDoc=%d (registro manual)",
			tablaFuente, cufe, rec.IdDoc)
		return Persistencia{Estado: EstadoProcesado, Manual: true}, nil
	}
	return c.persistManualTx(ctx, rec, cufe)
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
	cufe := strings.TrimSpace(data.CUFE)
	for _, a := range adjuntos {
		if len(a.Contenido) == 0 {
			c.log.Infof("    · BD: adjunto %q vacío, se omite su inserción", a.Nombre)
			continue
		}
		if err = c.insertAdjunto(ctx, tx, rec.Radicado, cufe, data.BL, a); err != nil {
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

// persistManualTx escribe el CUFE del XML en un registro manual, en transacción.
func (c *Client) persistManualTx(ctx context.Context, rec Radicado, cufe string) (_ Persistencia, err error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		c.log.Errorf("    · BD: no se pudo iniciar la transacción (manual): %v", err)
		return Persistencia{}, err
	}
	defer func() {
		if err != nil {
			if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
				c.log.Errorf("    · BD: error en rollback (manual): %v", rbErr)
			} else {
				c.log.Infof("    · BD: transacción revertida (manual, no se escribió nada)")
			}
		}
	}()

	query := `
UPDATE ` + c.tablaRadicado() + `
SET [Cufe/Cude] = @cufe
WHERE IdDoc = @iddoc`

	res, err := tx.ExecContext(ctx, query, sql.Named("cufe", cufe), sql.Named("iddoc", rec.IdDoc))
	if err != nil {
		c.log.Errorf("    · BD: error actualizando [Cufe/Cude] del registro manual IdDoc=%d: %v", rec.IdDoc, err)
		return Persistencia{}, err
	}
	if err = tx.Commit(); err != nil {
		c.log.Errorf("    · BD: error en commit (manual): %v", err)
		return Persistencia{}, err
	}
	n, _ := res.RowsAffected()
	c.log.Infof("    · BD: registro manual IdDoc=%d actualizado con [Cufe/Cude]='%s' (%d fila(s)) ✓", rec.IdDoc, cufe, n)
	return Persistencia{Estado: EstadoProcesado, Manual: true}, nil
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

	r, found, err := c.scanRadicado(ctx, query,
		sql.Named("cufe", cufe),
		sql.Named("ordenador", ordenadorAutom),
		sql.Named("usuario", usuarioAutom),
		sql.Named("pc", pcAutom),
		sql.Named("via", viaAutom),
	)
	if found {
		r.Manual = false
	}
	return r, found, err
}

// findManual localiza un registro MANUAL por NumDocumento+Prefijo+NIT (su CUFE
// suele venir vacío, por lo que no puede hallarse por CUFE — ajuste req. 4).
//
// Detalles del formato de la BD (validados con el cliente):
//   - El filtro de exclusión de automáticos combina ViaDeRecepcion <> 'MASIVA FE'
//     Y Ordenador <> 'DIANFE': así un registro ya procesado (que el UPDATE deja
//     en ViaDeRecepcion='EMAIL' pero conserva Ordenador='DIANFE') nunca vuelve a
//     ser tocado por el camino manual. Los manuales tienen vías variadas
//     ('CORREO JTABARES', 'CORREO FACTURAE', …), no un valor fijo.
//   - Prefijo puede venir como espacio en blanco (' '); se compara con TRIM.
//   - Nit se almacena como entero; se compara con CAST(... AS bigint) contra el
//     NIT del XML ya normalizado (sin puntos ni dígito de verificación).
func (c *Client) findManual(ctx context.Context, numDoc, prefijo string, nit int64) (Radicado, bool, error) {
	query := `
SELECT TOP 1 ` + columnasRadicado + `
FROM ` + c.tablaRadicado() + `
WHERE LTRIM(RTRIM(NumDocumento)) = @num
  AND LTRIM(RTRIM(Prefijo)) = @prefijo
  AND CAST(Nit AS bigint) = @nit
  AND ViaDeRecepcion <> @via
  AND Ordenador <> @ordenador`

	r, found, err := c.scanRadicado(ctx, query,
		sql.Named("num", numDoc),
		sql.Named("prefijo", prefijo),
		sql.Named("nit", nit),
		sql.Named("via", viaAutom),
		sql.Named("ordenador", ordenadorAutom),
	)
	if found {
		r.Manual = true
	}
	return r, found, err
}

// normalizeNITNumeric convierte el NIT tal como llega en el XML —que puede traer
// puntos y dígito de verificación, p.ej. "900.123.456-7"— al entero base que
// almacena Man_RadicadoFacturas_Test (901791583). Descarta el dígito de
// verificación (todo lo que sigue a un '-') y cualquier separador no numérico.
// Devuelve ok=false si no queda ningún dígito o el valor no cabe en int64.
func normalizeNITNumeric(nit string) (int64, bool) {
	s := strings.TrimSpace(nit)
	if i := strings.IndexByte(s, '-'); i >= 0 {
		s = s[:i] // descarta el dígito de verificación
	}
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteByte(byte(r))
		}
	}
	if b.Len() == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(b.String(), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
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
func (c *Client) updateRadicado(ctx context.Context, tx *sql.Tx, rec Radicado, data invoice.Data, fechaCol time.Time) error {
	query := `
UPDATE ` + c.tablaRadicado() + `
SET ViaDeRecepcion    = @via,
    FechaHoraOriginal = @fecha,
    Usuario           = @usuario,
    Pc                = @pc,
    Mandato           = @mandato,
    Explicacion       = @explicacion
WHERE IdDoc = @iddoc`

	res, err := tx.ExecContext(ctx, query,
		sql.Named("via", viaRecepcion),
		sql.Named("fecha", fechaCol),
		sql.Named("usuario", usuario),
		sql.Named("pc", pc),
		sql.Named("mandato", strings.TrimSpace(data.Pedido)),       // ajuste req. 3
		sql.Named("explicacion", strings.TrimSpace(data.Declarac)), // ajuste req. 3
		sql.Named("iddoc", rec.IdDoc),
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	c.log.Infof("    · BD: UPDATE radicado IdDoc=%d (%d fila(s)) → ViaDeRecepcion='%s', FechaHoraOriginal='%s' (UTC-5), Usuario='%s', Pc='%s', Mandato='%s', Explicacion='%s'",
		rec.IdDoc, n, viaRecepcion, fechaCol.Format("2006-01-02 15:04:05"), usuario, pc, data.Pedido, data.Declarac)
	return nil
}

// insertAdjunto inserta un archivo (PDF o XML) en dbo.Adjuntos.
//
// Ajustes Módulo 3:
//   - req. 1: IdFuente = campo Radicado (antes IdDoc).
//   - req. 5: NotasAdjunto = BL extraído del PDF.
func (c *Client) insertAdjunto(ctx context.Context, tx *sql.Tx, idFuente, cufe, bl string, a Adjunto) error {
	query := `
INSERT INTO ` + c.tablaAdjuntos() + `
    (IdFuente, BaseDatosFuente, TablaFuente, NombreAdjunto, Adjunto, Extension, KlFuente, NotasAdjunto)
VALUES
    (@idfuente, @bd, @tabla, @nombre, @adjunto, @ext, @kl, @notas)`

	_, err := tx.ExecContext(ctx, query,
		sql.Named("idfuente", idFuente), // ajuste req. 1: valor de la columna Radicado
		sql.Named("bd", baseDatosFuente),
		sql.Named("tabla", tablaFuente),
		sql.Named("nombre", a.Nombre),
		sql.Named("adjunto", a.Contenido),
		sql.Named("ext", a.Extension),
		sql.Named("kl", cufe),
		sql.Named("notas", strings.TrimSpace(bl)), // ajuste req. 5: BL
	)
	if err != nil {
		return err
	}
	c.log.Infof("    · BD: INSERT Adjuntos OK (IdFuente=%s, %s '%s', %d bytes, NotasAdjunto='%s')",
		idFuente, a.Extension, a.Nombre, len(a.Contenido), bl)
	return nil
}

// logSimulacionAuto registra el UPDATE/INSERT que se harían para un registro
// automático, sin tocar la base.
func (c *Client) logSimulacionAuto(rec Radicado, data invoice.Data, fechaCol time.Time, adjuntos []Adjunto) {
	c.log.Infof("    · BD [SIMULACIÓN] UPDATE %s SET ViaDeRecepcion='%s', FechaHoraOriginal='%s' (hora Colombia UTC-5), Usuario='%s', Pc='%s', Mandato='%s', Explicacion='%s' WHERE IdDoc=%d",
		tablaFuente, viaRecepcion, fechaCol.Format("2006-01-02 15:04:05"), usuario, pc, data.Pedido, data.Declarac, rec.IdDoc)
	cufe := strings.TrimSpace(data.CUFE)
	for _, a := range adjuntos {
		if len(a.Contenido) == 0 {
			c.log.Infof("    · BD [SIMULACIÓN] adjunto %q vacío, se omitiría", a.Nombre)
			continue
		}
		c.log.Infof("    · BD [SIMULACIÓN] INSERT Adjuntos (IdFuente=%s, BaseDatosFuente='%s', TablaFuente='%s', NombreAdjunto='%s', Extension='%s', KlFuente='%s', NotasAdjunto='%s', Adjunto=%d bytes)",
			rec.Radicado, baseDatosFuente, tablaFuente, a.Nombre, a.Extension, cufe, data.BL, len(a.Contenido))
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

// splitNumero separa el consecutivo del prefijo. El XML/Gemini suelen entregar
// Numero con el prefijo pegado (p.ej. "FE470" con Prefijo "FE" → "470"); si no
// hay prefijo, devuelve el número tal cual.
func splitNumero(numero, prefijo string) (numDoc, pref string) {
	numero = strings.TrimSpace(numero)
	pref = strings.TrimSpace(prefijo)
	numDoc = numero
	if pref != "" {
		numDoc = strings.TrimPrefix(numero, pref)
	}
	return strings.TrimSpace(numDoc), pref
}
