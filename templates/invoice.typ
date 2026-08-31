// Invoice template. Input schema: see examples/invoice.json.
// The service drops the request body next to this file as data.json.

#let d = json("data.json")
#let cur = d.at("currency", default: "$")
#let money(v) = [#cur#v]

#set page(paper: "a4", margin: (x: 2cm, y: 1.8cm), footer: context [
  #set text(size: 8pt, fill: gray)
  #d.seller.name — Invoice #d.invoice_no
  #h(1fr)
  Page #counter(page).display() of #counter(page).final().first()
])
#set text(size: 10pt)

// ---- Header ----------------------------------------------------------
#grid(
  columns: (1fr, auto),
  [
    #text(size: 18pt, weight: "bold", fill: rgb("#1a56db"))[#d.seller.name]
    #v(2pt)
    #text(size: 9pt, fill: gray)[
      #d.seller.address.join([ \ ])
    ]
  ],
  align(right)[
    #text(size: 14pt, weight: "bold")[INVOICE]
    #v(2pt)
    #text(size: 9pt)[
      No: #d.invoice_no \
      Date: #d.date \
      Due: #d.due_date
    ]
  ],
)
#line(length: 100%, stroke: 0.5pt + gray)

// ---- Bill to ---------------------------------------------------------
#v(6pt)
#text(size: 9pt, fill: gray)[BILL TO]
#v(-4pt)
*#d.buyer.name* \
#d.buyer.address.join([ \ ])
#v(10pt)

// ---- Line items ------------------------------------------------------
#table(
  columns: (1fr, auto, auto, auto),
  align: (left, right, right, right),
  stroke: 0.4pt + gray.lighten(40%),
  fill: (_, y) => if y == 0 { rgb("#1a56db").lighten(88%) },
  table.header([*Description*], [*Qty*], [*Unit price*], [*Amount*]),
  ..for it in d.items {
    (it.description, str(it.qty), money(it.unit_price), money(it.amount))
  },
)

// ---- Totals ----------------------------------------------------------
#v(8pt)
#align(right)[
  #table(
    columns: (auto, auto),
    align: (left, right),
    stroke: none,
    inset: (x: 8pt, y: 2.5pt),
    [Subtotal], money(d.subtotal),
    [Tax (#d.tax_rate)], money(d.tax),
    table.hline(stroke: 0.6pt),
    [*Total due*], [*#money(d.total)*],
  )
]

// ---- Notes -----------------------------------------------------------
#if d.at("notes", default: none) != none [
  #v(14pt)
  #text(size: 9pt, fill: gray)[#d.notes]
]
