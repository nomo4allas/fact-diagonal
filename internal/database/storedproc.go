package database

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/nomo4allas/fact-diagonal/internal/invoice"
)

// El Stored Procedure que resuelve TODA la lógica del Módulo 3 es configurable
// (Client.spName; SP_NAME en config.env). Se invoca por RPC (el texto del comando
// es solo el nombre del SP) para poder leer sus parámetros de salida @Mensaje y
// @Resultado.

// Operaciones soportadas por el SP.
const (
	opBuscarCUFE      = 0 // buscar el radicado por CUFE
	opActualizar      = 1 // actualizar Man_RadicadoFacturas
	opInsertarAdjunto = 2 // insertar un adjunto
)

// Límites de longitud de los parámetros varchar del SP (se truncan si exceden).
const (
	maxMandato       = 6
	maxNombreAdjunto = 500
	maxNotasAdjunto  = 500
	maxExtension     = 10
	maxNIT           = 20
	maxNumDoc        = 20
	maxPrefijo       = 20
)

// EstadoBD resume el desenlace de la persistencia de una factura; el pipeline lo
// usa para decidir la carpeta destino del correo (Procesados/Pendientes/Errores).
type EstadoBD int

const (
	// EstadoNoHallado: la Operacion 0 no encontró el CUFE (@Resultado=0), o el
	// XML no trae CUFE. → Pendientes.
	EstadoNoHallado EstadoBD = iota
	// EstadoPendiente: se encontró el CUFE pero algún adjunto no se insertó
	// (@Resultado=0 en la Operacion 2). → Pendientes.
	EstadoPendiente
	// EstadoProcesado: CUFE encontrado y ambos adjuntos insertados OK.
	// → Procesados.
	EstadoProcesado
)

// Persistencia reporta el resultado de PersistInvoice para clasificar el correo.
type Persistencia struct {
	Estado   EstadoBD
	Radicado int // radicado devuelto por la Operacion 0 (0 si no se halló)
	Adjuntos int // número de adjuntos efectivamente insertados (@Resultado>0)
}

// Adjunto representa un archivo a insertar vía la Operacion 2 del SP. Puede ser
// cualquier archivo del ZIP: PDF, XML, JPG, TIF, DOCX, etc.
type Adjunto struct {
	Nombre    string // @NombreAdjunto: nombre exacto del archivo
	Extension string // @Extension: extensión sin punto ("pdf", "xml", "jpg"…)
	Contenido []byte // @Adjunto (varbinary)
}

// bogotaOffset es el desfase horario de Colombia (UTC-5, sin horario de verano).
const bogotaOffset = -5 * time.Hour

// aHoraColombia convierte un instante (las fechas de Graph llegan en UTC) a la
// hora de pared de Colombia (UTC-5). El resultado queda etiquetado como UTC a
// propósito: así el driver de SQL Server escribe esos componentes tal cual en
// columnas sin zona (datetime), sin volver a convertir a UTC.
func aHoraColombia(t time.Time) time.Time {
	return t.UTC().Add(bogotaOffset)
}

// spParams agrupa los parámetros de entrada del SP. Cada operación llena solo los
// que necesita; el resto viaja con su valor cero / NULL.
type spParams struct {
	Operacion         int
	Cufe              string
	Radicado          int
	FechaHoraOriginal time.Time
	Mandato           string
	NotasAdjunto      *string // nil → NULL
	NombreAdjunto     string
	Extension         string
	Adjunto           []byte // nil/vacío → NULL

	// Datos de respaldo de la Operacion 0: si el CUFE no aparece, el SP busca el
	// radicado por NIT + número de documento + prefijo. Los tres son varchar(20)
	// en el SP. Aquí solo se envían; el fallback (incluida la separación del
	// prefijo dentro de @NumDoc) lo resuelve el SP internamente.
	NIT     string // @nit: solo dígitos, sin dígito de verificación ("" → NULL)
	NumDoc  string // @NumDoc: número de documento tal cual ("" → NULL)
	Prefijo string // @Prefijo: prefijo del número de factura ("" → NULL)
}

// PersistInvoice ejecuta el flujo del Módulo 3 para una factura, orquestando las
// tres operaciones del SP, y devuelve el desenlace (Persistencia) para que el
// pipeline decida la carpeta destino.
//
// Flujo:
//  1. Operacion 0 (buscar por CUFE). @Resultado=0 → EstadoNoHallado (Pendientes).
//     @Resultado>0 → es el radicado; se continúa.
//  2. Operacion 1 (actualizar). Si devuelve 0 se registra advertencia pero se
//     sigue con los adjuntos.
//  3. Operacion 2 (insertar adjunto) por cada archivo del ZIP (PDF, XML, JPG,
//     TIF, DOCX…), todos con @NotasAdjunto = número de documento. Si alguno
//     devuelve 0 → EstadoPendiente. Todos OK → EstadoProcesado.
//
// Las Operaciones 1 y 2 solo se ejecutan si la Operacion 0 halló el CUFE
// (Radicado > 0); en caso contrario → EstadoNoHallado (Pendientes).
//
// Un error técnico al invocar el SP se propaga (el pipeline lo clasifica como
// Errores). En SIMULATION_MODE no se llama al SP: solo se loguean los parámetros.
func (c *Client) PersistInvoice(ctx context.Context, data invoice.Data, fechaCorreo time.Time, adjuntos []Adjunto) (Persistencia, error) {
	cufe := strings.TrimSpace(data.CUFE)
	if cufe == "" {
		c.log.Infof("    · BD: XML sin CUFE; no se puede invocar la búsqueda (Operacion 0) → pendiente")
		return Persistencia{Estado: EstadoNoHallado}, nil
	}

	fechaCol := aHoraColombia(fechaCorreo)
	mandato := truncar(strings.TrimSpace(data.Pedido), maxMandato)

	if c.simulation {
		return c.simular(data, cufe, fechaCol, mandato, adjuntos), nil
	}

	// 1) Operacion 0 — buscar por CUFE (con los datos de respaldo por si el SP
	// necesita caer al fallback NIT + número + prefijo).
	nit, numDoc, prefijo := datosBusqueda(data)
	radicado, msg, err := c.callSP(ctx, spParams{
		Operacion:         opBuscarCUFE,
		Cufe:              cufe,
		FechaHoraOriginal: fechaCol,
		NIT:               nit,
		NumDoc:            numDoc,
		Prefijo:           prefijo,
	})
	if err != nil {
		c.log.Errorf("    · BD: Operacion 0 (buscar CUFE) falló: %v", err)
		return Persistencia{}, err
	}
	c.log.Infof("    · BD: Operacion 0 (buscar CUFE=%s, @nit=%s, @NumDoc=%s, @Prefijo=%s) → Radicado=%d, Mensaje=%q",
		cufe, orNULL(nit), orNULL(numDoc), orNULL(prefijo), radicado, msg)
	if radicado <= 0 {
		c.log.Infof("    · BD: CUFE no encontrado (Operacion 0 devolvió 0) → pendiente")
		return Persistencia{Estado: EstadoNoHallado}, nil
	}

	// 2) Operacion 1 — actualizar Man_RadicadoFacturas.
	res1, msg1, err := c.callSP(ctx, spParams{
		Operacion:         opActualizar,
		Cufe:              cufe,
		Radicado:          radicado,
		FechaHoraOriginal: fechaCol,
		Mandato:           mandato,
	})
	if err != nil {
		c.log.Errorf("    · BD: Operacion 1 (actualizar) falló: %v", err)
		return Persistencia{}, err
	}
	if res1 <= 0 {
		c.log.Infof("    · BD: ⚠ Operacion 1 (actualizar Radicado=%d) devolvió 0 (Mensaje=%q); se continúa con los adjuntos", radicado, msg1)
	} else {
		c.log.Infof("    · BD: Operacion 1 (actualizar Radicado=%d) OK → Resultado=%d", radicado, res1)
	}

	// 3) Operacion 2 — insertar todos los adjuntos del ZIP (PDF, XML y demás).
	// @NotasAdjunto lleva el número de documento de la factura para todos.
	notas := notasParaAdjunto(data.Numero)
	insertados, todosOK := 0, true
	for _, a := range adjuntos {
		if len(a.Contenido) == 0 {
			c.log.Infof("    · BD: adjunto %q vacío, se omite su inserción", a.Nombre)
			todosOK = false
			continue
		}
		res2, msg2, err := c.callSP(ctx, spParams{
			Operacion:     opInsertarAdjunto,
			Cufe:          cufe,
			Radicado:      radicado,
			NombreAdjunto: truncar(a.Nombre, maxNombreAdjunto),
			Extension:     truncar(a.Extension, maxExtension),
			Adjunto:       a.Contenido,
			NotasAdjunto:  notas,
		})
		if err != nil {
			c.log.Errorf("    · BD: Operacion 2 (insertar %s %q) falló: %v", a.Extension, a.Nombre, err)
			return Persistencia{}, err
		}
		// El SP marca "ya registrado" cuando el adjunto ya se había insertado en
		// una corrida anterior. El SP de producción devuelve ese caso con
		// @Resultado = IdAdjunto (>0); antes lo devolvía con @Resultado=0. En
		// ambos casos el adjunto está en la BD (éxito idempotente), por lo que el
		// fallo real es solo @Resultado=0 SIN el mensaje de "ya registrado".
		if !adjuntoPersistido(res2, msg2) {
			c.log.Infof("    · BD: ⚠ Operacion 2 (insertar %s %q) devolvió 0 (Mensaje=%q)", a.Extension, a.Nombre, msg2)
			todosOK = false
			continue
		}
		if adjuntoYaRegistrado(msg2) {
			c.log.Infof("    · BD: Operacion 2 (insertar %s %q) → adjunto ya registrado previamente (Resultado=%d, Mensaje=%q); se trata como éxito", a.Extension, a.Nombre, res2, msg2)
		} else {
			c.log.Infof("    · BD: Operacion 2 (insertar %s %q, %d bytes) OK → Resultado=%d", a.Extension, a.Nombre, len(a.Contenido), res2)
		}
		insertados++
	}

	estado := EstadoProcesado
	if !todosOK || insertados == 0 {
		estado = EstadoPendiente
	}
	return Persistencia{Estado: estado, Radicado: radicado, Adjuntos: insertados}, nil
}

// callSP invoca el Stored Procedure por RPC con los parámetros que aplican a la
// operación (los que no aplican viajan como NULL) y lee los de salida @Mensaje y
// @Resultado.
//
// Reparto de parámetros por operación:
//   - @Cufe: aplica a las tres operaciones. En la inserción de adjunto (2) el SP
//     lo usa para validar que el registro exista antes de insertar.
//   - @FechaHoraOriginal y @Mandato: aplican a la búsqueda (0) y la
//     actualización (1); NULL en la inserción de adjunto (2).
//   - @nit, @NumDoc y @Prefijo: solo aplican a la búsqueda (0), donde el SP los
//     usa como respaldo si el CUFE no aparece; NULL en las demás operaciones.
//   - @NotasAdjunto, @NombreAdjunto, @Extension y @Adjunto: solo aplican a la
//     inserción de adjunto (2); NULL en las demás operaciones.
//
// Los sql.Named se listan en el mismo orden que la firma del SP por legibilidad;
// el enlace es por nombre, así que el orden no altera la llamada.
func (c *Client) callSP(ctx context.Context, p spParams) (resultado int, mensaje string, err error) {
	// @Cufe viaja en las tres operaciones (el SP lo valida también en la Op 2).
	cufe := any(p.Cufe)

	// @FechaHoraOriginal y @Mandato: NULL en la Operacion 2 (insertar adjunto).
	var fechaHora, mandato any // nil → NULL
	if p.Operacion != opInsertarAdjunto {
		fechaHora = p.FechaHoraOriginal
		mandato = p.Mandato
	}

	// Datos de respaldo: solo en la Operacion 0 (buscar).
	nit, numDoc, prefijo := p.argsBusqueda()

	// Campos de adjunto: NULL salvo en la Operacion 2 (insertar adjunto).
	var notas, nombreAdjunto, extension, adjunto any // nil → NULL
	if p.Operacion == opInsertarAdjunto {
		if p.NotasAdjunto != nil {
			notas = *p.NotasAdjunto
		}
		nombreAdjunto = p.NombreAdjunto
		extension = p.Extension
		if len(p.Adjunto) > 0 {
			adjunto = p.Adjunto
		}
	}

	_, err = c.db.ExecContext(ctx, c.spName,
		sql.Named("Operacion", p.Operacion),
		sql.Named("Cufe", cufe),
		sql.Named("Radicado", p.Radicado),
		sql.Named("nit", nit),
		sql.Named("NumDoc", numDoc),
		sql.Named("Prefijo", prefijo),
		sql.Named("FechaHoraOriginal", fechaHora),
		sql.Named("Mandato", mandato),
		sql.Named("NotasAdjunto", notas),
		sql.Named("NombreAdjunto", nombreAdjunto),
		sql.Named("Extension", extension),
		sql.Named("Adjunto", adjunto),
		sql.Named("Mensaje", sql.Out{Dest: &mensaje}),
		sql.Named("Resultado", sql.Out{Dest: &resultado}),
	)
	if err != nil {
		return 0, "", err
	}
	return resultado, mensaje, nil
}

// simular registra los parámetros que se enviarían al SP en cada operación, sin
// ejecutarlo. Como no se llama a la Operacion 0, no hay radicado real: se usa el
// marcador 0 y se asume el desenlace que tendría un flujo exitoso, para que la
// clasificación de carpetas en simulación sea representativa.
func (c *Client) simular(data invoice.Data, cufe string, fechaCol time.Time, mandato string, adjuntos []Adjunto) Persistencia {
	nit, numDoc, prefijo := datosBusqueda(data)
	c.log.Infof("    · BD [SIMULACIÓN] Operacion 0 (buscar) → SP %s(@Operacion=0, @Cufe=%s, @nit=%s, @NumDoc=%s, @Prefijo=%s)",
		c.spName, cufe, orNULL(nit), orNULL(numDoc), orNULL(prefijo))
	c.log.Infof("    · BD [SIMULACIÓN] Operacion 1 (actualizar) → SP %s(@Operacion=1, @Radicado=<Op0>, @Cufe=%s, @FechaHoraOriginal='%s' (UTC-5), @Mandato=%s)",
		c.spName, cufe, fechaCol.Format("2006-01-02 15:04:05"), orNULL(mandato))

	notas := notasParaAdjunto(data.Numero)
	insertables := 0
	for _, a := range adjuntos {
		if len(a.Contenido) == 0 {
			c.log.Infof("    · BD [SIMULACIÓN] adjunto %q vacío, se omitiría", a.Nombre)
			continue
		}
		c.log.Infof("    · BD [SIMULACIÓN] Operacion 2 (insertar) → SP %s(@Operacion=2, @Radicado=<Op0>, @Cufe=%s, @NombreAdjunto=%s, @Extension=%s, @NotasAdjunto=%s, @Adjunto=%d bytes)",
			c.spName, cufe, truncar(a.Nombre, maxNombreAdjunto), truncar(a.Extension, maxExtension), notasLog(notas), len(a.Contenido))
		insertables++
	}

	estado := EstadoProcesado
	if insertables == 0 {
		estado = EstadoPendiente
	}
	return Persistencia{Estado: estado, Adjuntos: insertables}
}

// notasParaAdjunto devuelve el valor de @NotasAdjunto: el número de documento de
// la factura (truncado) para TODOS los adjuntos —PDF, XML, JPG, TIF, DOCX…—, de
// modo que todos queden referenciados al mismo documento; NULL (nil) si el
// número de documento no está disponible.
func notasParaAdjunto(numDocumento string) *string {
	s := strings.TrimSpace(numDocumento)
	if s == "" {
		return nil
	}
	s = truncar(s, maxNotasAdjunto)
	return &s
}

// argsBusqueda reparte @nit, @NumDoc y @Prefijo: llevan valor solo en la
// Operacion 0 (buscar), que es donde el SP los usa como respaldo del CUFE, y
// viajan NULL en las Operaciones 1 y 2 aunque spParams los traiga poblados.
//
// Dentro de la Operacion 0 cada uno es independiente: el que no se pudo obtener
// va NULL, para que el SP no intente un fallback con un valor vacío que podría
// emparejar el radicado equivocado.
func (p spParams) argsBusqueda() (nit, numDoc, prefijo any) { // nil → NULL
	if p.Operacion != opBuscarCUFE {
		return nil, nil, nil
	}
	if p.NIT != "" {
		nit = p.NIT
	}
	if p.NumDoc != "" {
		numDoc = p.NumDoc
	}
	if p.Prefijo != "" {
		prefijo = p.Prefijo
	}
	return nit, numDoc, prefijo
}

// datosBusqueda arma los tres datos de respaldo que la Operacion 0 envía junto
// al CUFE (@nit, @NumDoc, @Prefijo) para que el SP pueda localizar el radicado
// por NIT + número + prefijo cuando el CUFE no aparece. Los tres son varchar(20)
// en el SP; aquí solo se recortan a ese límite y se envían: la lógica de
// búsqueda vive dentro del SP.
//
// @NumDoc va tal como lo dejó el extractor ("FES15380" se envía completo, con su
// prefijo): es el SP el que separa prefijo y consecutivo.
//
// @Prefijo se pasa a mayúsculas: en la BD los prefijos están en mayúsculas y la
// comparación del SP puede ser sensible a mayúsculas.
//
// Cada dato es independiente: si alguno no se pudo obtener viaja como NULL y los
// demás igual se envían.
func datosBusqueda(data invoice.Data) (nit, numDoc, prefijo string) {
	nit = truncar(normalizeNIT(data.NIT), maxNIT)
	numDoc = truncar(strings.TrimSpace(data.Numero), maxNumDoc)
	prefijo = truncar(strings.ToUpper(strings.TrimSpace(data.Prefijo)), maxPrefijo)
	return nit, numDoc, prefijo
}

// normalizeNIT deja el NIT del emisor listo para @nit: solo los dígitos, sin
// puntos ni espacios y sin el dígito de verificación ("900.123.456-7" →
// "900123456"). El DV es el que sigue al último guion; si el NIT llega sin guion
// se asume que ya viene sin DV ("901234567" → "901234567"), que es como lo trae
// el cbc:CompanyID del XML de la DIAN. Devuelve "" si no queda ningún dígito.
func normalizeNIT(nit string) string {
	s := strings.TrimSpace(nit)
	if i := strings.LastIndex(s, "-"); i >= 0 {
		s = s[:i]
	}
	var b strings.Builder
	for _, r := range s {
		if esDigito(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// esDigito acota la comprobación a los dígitos ASCII: unicode.IsDigit aceptaría
// dígitos de otros alfabetos que luego no se podrían convertir a int.
func esDigito(r rune) bool { return r >= '0' && r <= '9' }

// adjuntoPersistido decide si el resultado de la Operacion 2 debe contarse como
// un adjunto efectivamente persistido en la BD. Es éxito si el SP devolvió un
// IdAdjunto (@Resultado>0) o si el mensaje indica que el adjunto ya estaba
// registrado de una corrida previa (éxito idempotente), sin importar el valor de
// @Resultado. El único fallo real es @Resultado=0 con un mensaje que NO contiene
// "ya registrado".
func adjuntoPersistido(resultado int, mensaje string) bool {
	return resultado > 0 || adjuntoYaRegistrado(mensaje)
}

// adjuntoYaRegistrado indica si el mensaje del SP para la Operacion 2 señala que
// el adjunto ya existía en la BD (insertado en una corrida previa). Es un éxito
// idempotente: el SP de producción lo reporta con @Resultado = IdAdjunto (>0),
// aunque antes lo hacía con @Resultado=0; por eso la decisión se basa en el
// mensaje, no en @Resultado. La comparación es insensible a mayúsculas/minúsculas.
func adjuntoYaRegistrado(mensaje string) bool {
	return strings.Contains(strings.ToLower(mensaje), "ya registrado")
}

// truncar recorta s a un máximo de max runas (los límites varchar del SP), sin
// partir caracteres multibyte.
func truncar(s string, max int) string {
	r := []rune(s)
	if len(r) > max {
		return string(r[:max])
	}
	return s
}

// orNULL formatea una cadena para el log de simulación: su valor o "NULL" si viene vacía.
func orNULL(s string) string {
	if strings.TrimSpace(s) == "" {
		return "NULL"
	}
	return s
}

// notasLog formatea @NotasAdjunto para el log de simulación: 'valor' o NULL.
func notasLog(notas *string) string {
	if notas == nil {
		return "NULL"
	}
	return "'" + *notas + "'"
}
