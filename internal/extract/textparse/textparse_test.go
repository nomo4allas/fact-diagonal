package textparse

import "testing"

func TestParseTextoFactura(t *testing.T) {
	text := `REPRESENTACION GRAFICA FACTURA ELECTRONICA DE VENTA
Proveedor Ejemplo SAS
NIT: 900.123.456-7
Factura No. FE-470
Fecha de emisión: 2026-06-20
Total a pagar $ 119.000,00
CUFE: a1b2c3d4e5f60718293a4b5c6d7e8f90112233445566778899aabbccddeeff00112233445566778899aabbccddeeff00`

	d := Parse(text)

	if d.NIT != "900.123.456-7" {
		t.Errorf("NIT = %q", d.NIT)
	}
	if d.Numero != "FE470" {
		t.Errorf("Numero = %q, want FE470", d.Numero)
	}
	if d.Prefijo != "FE" {
		t.Errorf("Prefijo = %q, want FE", d.Prefijo)
	}
	if d.FechaEmision != "2026-06-20" {
		t.Errorf("FechaEmision = %q", d.FechaEmision)
	}
	if d.ValorTotal != "119.000,00" {
		t.Errorf("ValorTotal = %q, want 119.000,00", d.ValorTotal)
	}
	if len(d.CUFE) < 90 {
		t.Errorf("CUFE = %q", d.CUFE)
	}
}
