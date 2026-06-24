// Package xmldoc extrae los campos de una factura desde su XML UBL 2.1 (DIAN).
//
// Maneja las dos envolturas habituales:
//   - Invoice: el documento UBL de la factura propiamente dicho.
//   - AttachedDocument: contenedor que lleva la factura embebida como texto
//     dentro de un CDATA (cbc:Description); en ese caso se desempaqueta y se
//     vuelve a parsear el Invoice interno.
//
// El emparejado de elementos es por nombre local, de modo que los prefijos de
// namespace (cbc:, cac:, …) no afectan al análisis.
package xmldoc

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"

	"github.com/nomo4allas/fact-diagonal/internal/invoice"
)

// ublInvoice mapea los campos de interés del documento UBL Invoice. Los tags
// usan solo el nombre local del elemento; encoding/xml empareja por nombre
// local independientemente del namespace.
type ublInvoice struct {
	ID         string `xml:"ID"`        // número de factura (con prefijo)
	UUID       string `xml:"UUID"`      // CUFE
	IssueDate  string `xml:"IssueDate"` // fecha de emisión (YYYY-MM-DD)
	IssueTime  string `xml:"IssueTime"`
	Prefijo    string `xml:"Prefijo"` // no estándar; algunos emisores lo incluyen
	Supplier   party  `xml:"AccountingSupplierParty"`
	LegalTotal struct {
		PayableAmount       string `xml:"PayableAmount"`
		LineExtensionAmount string `xml:"LineExtensionAmount"`
		TaxInclusiveAmount  string `xml:"TaxInclusiveAmount"`
	} `xml:"LegalMonetaryTotal"`
}

type party struct {
	Party struct {
		PartyName []struct {
			Name string `xml:"Name"`
		} `xml:"PartyName"`
		PartyTaxScheme []struct {
			RegistrationName string `xml:"RegistrationName"`
			CompanyID        string `xml:"CompanyID"`
		} `xml:"PartyTaxScheme"`
		PartyLegalEntity []struct {
			RegistrationName string `xml:"RegistrationName"`
			CompanyID        string `xml:"CompanyID"`
		} `xml:"PartyLegalEntity"`
	} `xml:"Party"`
}

// attachedDocument captura el/los CDATA que pueden contener el Invoice embebido.
type attachedDocument struct {
	Descriptions []string `xml:"Attachment>ExternalReference>Description"`
}

// Parse analiza el XML de una factura y devuelve sus datos. Si recibe un
// AttachedDocument, desempaqueta el Invoice interno y lo analiza.
func Parse(data []byte) (invoice.Data, error) {
	data = stripBOM(data)

	root, err := rootLocalName(data)
	if err != nil {
		return invoice.Data{}, err
	}

	if strings.EqualFold(root, "AttachedDocument") {
		inner, err := embeddedInvoice(data)
		if err != nil {
			return invoice.Data{}, err
		}
		return Parse(inner)
	}

	var inv ublInvoice
	if err := xml.Unmarshal(data, &inv); err != nil {
		return invoice.Data{}, fmt.Errorf("error parseando XML UBL: %w", err)
	}

	d := invoice.Data{
		Numero:       strings.TrimSpace(inv.ID),
		Prefijo:      strings.TrimSpace(inv.Prefijo),
		CUFE:         strings.TrimSpace(inv.UUID),
		FechaEmision: strings.TrimSpace(inv.IssueDate),
		NIT:          supplierNIT(inv.Supplier),
		RazonSocial:  supplierName(inv.Supplier),
		ValorTotal:   firstNonEmpty(inv.LegalTotal.PayableAmount, inv.LegalTotal.TaxInclusiveAmount, inv.LegalTotal.LineExtensionAmount),
		Source:       invoice.SourceXML,
	}
	d.DerivePrefijo()
	return d, nil
}

// supplierNIT toma el CompanyID del PartyTaxScheme y, en su defecto, del
// PartyLegalEntity.
func supplierNIT(p party) string {
	for _, ts := range p.Party.PartyTaxScheme {
		if v := strings.TrimSpace(ts.CompanyID); v != "" {
			return v
		}
	}
	for _, le := range p.Party.PartyLegalEntity {
		if v := strings.TrimSpace(le.CompanyID); v != "" {
			return v
		}
	}
	return ""
}

// supplierName prioriza PartyName/Name y cae a las razones sociales del
// PartyLegalEntity o PartyTaxScheme.
func supplierName(p party) string {
	for _, pn := range p.Party.PartyName {
		if v := strings.TrimSpace(pn.Name); v != "" {
			return v
		}
	}
	for _, le := range p.Party.PartyLegalEntity {
		if v := strings.TrimSpace(le.RegistrationName); v != "" {
			return v
		}
	}
	for _, ts := range p.Party.PartyTaxScheme {
		if v := strings.TrimSpace(ts.RegistrationName); v != "" {
			return v
		}
	}
	return ""
}

// embeddedInvoice extrae, de un AttachedDocument, el contenido CDATA que
// corresponde al Invoice embebido.
func embeddedInvoice(data []byte) ([]byte, error) {
	var ad attachedDocument
	if err := xml.Unmarshal(data, &ad); err != nil {
		return nil, fmt.Errorf("error parseando AttachedDocument: %w", err)
	}
	for _, desc := range ad.Descriptions {
		if strings.Contains(desc, "<Invoice") || strings.Contains(desc, ":Invoice") {
			return []byte(strings.TrimSpace(desc)), nil
		}
	}
	return nil, fmt.Errorf("AttachedDocument sin Invoice embebido en Description")
}

// rootLocalName devuelve el nombre local del elemento raíz del documento.
func rootLocalName(data []byte) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("XML sin elemento raíz: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se.Name.Local, nil
		}
	}
}

func stripBOM(data []byte) []byte {
	return bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
