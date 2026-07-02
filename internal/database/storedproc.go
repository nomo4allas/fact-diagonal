package database

import (
	"context"
	"database/sql"
	"strconv"
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
	maxExtension     = 6
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

// Adjunto representa un archivo a insertar vía la Operacion 2 del SP.
type Adjunto struct {
	Nombre    string // @NombreAdjunto
	Extension string // @Extension: "pdf" | "xml"
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
	Nit               string  // @nit (tipo_nit; se envía como varchar)
	Valor             float64 // @valor numeric(18,2)
	FechaHoraOriginal time.Time
	Mandato           string
	NotasAdjunto      *string // nil → NULL
	NombreAdjunto     string
	Extension         string
	Adjunto           []byte // nil/vacío → NULL
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
//  3. Operacion 2 (insertar adjunto) por cada adjunto (PDF y XML). Si alguno
//     devuelve 0 → EstadoPendiente. Todos OK → EstadoProcesado.
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
	nit := strings.TrimSpace(data.NIT)
	valor := parseValor(data.ValorTotal)
	mandato := truncar(strings.TrimSpace(data.Pedido), maxMandato)

	if c.simulation {
		return c.simular(cufe, nit, valor, fechaCol, mandato, data.BL, adjuntos), nil
	}

	// 1) Operacion 0 — buscar por CUFE.
	radicado, msg, err := c.callSP(ctx, spParams{
		Operacion:         opBuscarCUFE,
		Cufe:              cufe,
		FechaHoraOriginal: fechaCol,
	})
	if err != nil {
		c.log.Errorf("    · BD: Operacion 0 (buscar CUFE) falló: %v", err)
		return Persistencia{}, err
	}
	c.log.Infof("    · BD: Operacion 0 (buscar CUFE=%s) → Radicado=%d, Mensaje=%q", cufe, radicado, msg)
	if radicado <= 0 {
		c.log.Infof("    · BD: CUFE no encontrado (Operacion 0 devolvió 0) → pendiente")
		return Persistencia{Estado: EstadoNoHallado}, nil
	}

	// 2) Operacion 1 — actualizar Man_RadicadoFacturas.
	res1, msg1, err := c.callSP(ctx, spParams{
		Operacion:         opActualizar,
		Cufe:              cufe,
		Radicado:          radicado,
		Nit:               nit,
		Valor:             valor,
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

	// 3) Operacion 2 — insertar adjuntos (PDF y XML).
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
			NotasAdjunto:  notasParaAdjunto(a.Extension, data.BL),
		})
		if err != nil {
			c.log.Errorf("    · BD: Operacion 2 (insertar %s %q) falló: %v", a.Extension, a.Nombre, err)
			return Persistencia{}, err
		}
		if res2 <= 0 {
			c.log.Infof("    · BD: ⚠ Operacion 2 (insertar %s %q) devolvió 0 (Mensaje=%q)", a.Extension, a.Nombre, msg2)
			todosOK = false
			continue
		}
		c.log.Infof("    · BD: Operacion 2 (insertar %s %q, %d bytes) OK → Resultado=%d", a.Extension, a.Nombre, len(a.Contenido), res2)
		insertados++
	}

	estado := EstadoProcesado
	if !todosOK || insertados == 0 {
		estado = EstadoPendiente
	}
	return Persistencia{Estado: estado, Radicado: radicado, Adjuntos: insertados}, nil
}

// callSP invoca el Stored Procedure por RPC con TODOS sus parámetros de entrada
// (los no usados por la operación viajan con su valor cero / NULL) y lee los de
// salida @Mensaje y @Resultado.
func (c *Client) callSP(ctx context.Context, p spParams) (resultado int, mensaje string, err error) {
	var notas any // nil → NULL
	if p.NotasAdjunto != nil {
		notas = *p.NotasAdjunto
	}
	var adjunto any // nil → NULL
	if len(p.Adjunto) > 0 {
		adjunto = p.Adjunto
	}

	_, err = c.db.ExecContext(ctx, c.spName,
		sql.Named("Operacion", p.Operacion),
		sql.Named("Cufe", p.Cufe),
		sql.Named("Radicado", p.Radicado),
		sql.Named("nit", p.Nit),
		sql.Named("valor", p.Valor),
		sql.Named("FechaHoraOriginal", p.FechaHoraOriginal),
		sql.Named("Mandato", p.Mandato),
		sql.Named("NotasAdjunto", notas),
		sql.Named("NombreAdjunto", p.NombreAdjunto),
		sql.Named("Extension", p.Extension),
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
func (c *Client) simular(cufe, nit string, valor float64, fechaCol time.Time, mandato, bl string, adjuntos []Adjunto) Persistencia {
	c.log.Infof("    · BD [SIMULACIÓN] Operacion 0 (buscar) → SP %s(@Operacion=0, @Cufe=%s)", c.spName, cufe)
	c.log.Infof("    · BD [SIMULACIÓN] Operacion 1 (actualizar) → SP %s(@Operacion=1, @Radicado=<Op0>, @Cufe=%s, @nit=%s, @valor=%.2f, @FechaHoraOriginal='%s' (UTC-5), @Mandato=%s)",
		c.spName, cufe, orNULL(nit), valor, fechaCol.Format("2006-01-02 15:04:05"), orNULL(mandato))

	insertables := 0
	for _, a := range adjuntos {
		if len(a.Contenido) == 0 {
			c.log.Infof("    · BD [SIMULACIÓN] adjunto %q vacío, se omitiría", a.Nombre)
			continue
		}
		notas := notasParaAdjunto(a.Extension, bl)
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

// notasParaAdjunto devuelve el valor de @NotasAdjunto: el BL (truncado) para el
// PDF si trae valor; NULL (nil) para el XML o cuando el BL viene vacío.
func notasParaAdjunto(extension, bl string) *string {
	if !strings.EqualFold(strings.TrimSpace(extension), "pdf") {
		return nil
	}
	s := strings.TrimSpace(bl)
	if s == "" {
		return nil
	}
	s = truncar(s, maxNotasAdjunto)
	return &s
}

// parseValor convierte el valor total de la factura (cadena) a float64 para
// @valor. Soporta el formato del XML UBL ("119000.00") y el colombiano del texto
// nativo ("119.000,00"); ante un valor no parseable devuelve 0.
func parseValor(s string) float64 {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if (r >= '0' && r <= '9') || r == '.' || r == ',' || r == '-' {
			b.WriteRune(r)
		}
	}
	v := b.String()
	if v == "" {
		return 0
	}

	lastDot := strings.LastIndexByte(v, '.')
	lastComma := strings.LastIndexByte(v, ',')
	switch {
	case lastDot >= 0 && lastComma >= 0:
		// El separador decimal es el que aparece más a la derecha; el otro es de miles.
		if lastComma > lastDot {
			v = strings.ReplaceAll(v, ".", "")
			v = strings.ReplaceAll(v, ",", ".")
		} else {
			v = strings.ReplaceAll(v, ",", "")
		}
	case lastComma >= 0:
		// Solo comas: una sola coma con <=2 decimales es separador decimal; si no, miles.
		if strings.Count(v, ",") == 1 && len(v)-lastComma-1 <= 2 {
			v = strings.ReplaceAll(v, ",", ".")
		} else {
			v = strings.ReplaceAll(v, ",", "")
		}
	default:
		// Solo puntos: varios puntos son separadores de miles; uno solo se deja como decimal.
		if strings.Count(v, ".") > 1 {
			v = strings.ReplaceAll(v, ".", "")
		}
	}

	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0
	}
	return f
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
