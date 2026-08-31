// Formal certificate template (authorization, completion, membership …).
// Input schema: see examples/certificate.json.
// The service drops the request body next to this file as data.json.
//
// Generic by design: it carries no government seal, official signature, or
// verification URL of any real issuing authority. Supply your own issuer.

#let d = json("data.json")
#let accent = rgb(d.at("accent_color", default: "#1f6f43"))

#set page(paper: "us-letter", margin: 2cm)
#set text(size: 12pt, font: "Libertinus Serif")
#set align(center)

// ---- Decorative double border ----------------------------------------
#box(
  width: 100%,
  height: 100%,
  stroke: 2pt + accent,
  inset: 6pt,
)[
  #box(width: 100%, height: 100%, stroke: 0.75pt + accent, inset: 2.2cm)[
    #set align(center + horizon)
    #block[
      #text(size: 13pt, weight: "bold", tracking: 2pt)[#upper(d.issuer.name)]
      #if d.issuer.at("subtitle", default: none) != none [
        #v(-6pt)
        #text(size: 10pt, fill: gray)[#d.issuer.subtitle]
      ]

      #v(1.4cm)
      #text(size: 22pt, weight: "bold", tracking: 1pt)[#upper(d.title)]

      #v(1cm)
      #set align(center)
      #set par(justify: false, leading: 0.9em)
      #text(size: 12.5pt)[#d.body]

      #if d.at("subject", default: none) != none [
        #v(0.8cm)
        #text(size: 18pt, weight: "bold")[#d.subject]
        #if d.at("subject_note", default: none) != none [
          #v(-4pt)
          #text(size: 11pt, fill: gray)[#d.subject_note]
        ]
      ]

      #if d.at("statement", default: none) != none [
        #v(0.8cm)
        #text(size: 12.5pt)[#d.statement]
      ]

      // ---- Signature + reference block --------------------------------
      #v(2cm)
      #grid(
        columns: (1fr, 1fr),
        gutter: 1cm,
        align: (left + bottom, right + bottom),
        [
          #line(length: 5.5cm, stroke: 0.6pt)
          #v(-6pt)
          #text(size: 11pt)[
            *#d.signatory.name* \
            #d.signatory.title
          ]
        ],
        align(right)[
          #set text(size: 9pt, fill: gray)
          #set align(right)
          Issued: #d.issue_date \
          #for r in d.at("references", default: ()) [
            #r.label: #r.value \
          ]
        ],
      )
    ]
  ]
]
