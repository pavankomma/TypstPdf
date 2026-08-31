// Structured legal-document template (articles, bylaws, operating
// agreements …) — numbered articles, each a heading plus labeled fields
// and/or body paragraphs, with a header block and an execution line.
// Input schema: see examples/filing.json.
// The service drops the request body next to this file as data.json.
//
// Generic by design: it reproduces no real filing office's letterhead,
// seal, or filing identifiers. Supply your own header details.

#let d = json("data.json")

#set page(paper: "us-letter", margin: (x: 2.2cm, y: 2cm), footer: context [
  #set text(size: 8pt, fill: gray)
  #d.at("footer", default: d.title)
  #h(1fr)
  Page #counter(page).display() of #counter(page).final().first()
])
#set text(size: 11pt, font: "Libertinus Serif")
#set par(justify: true, leading: 0.7em)

// ---- Header ----------------------------------------------------------
#grid(
  columns: (1fr, auto),
  align: (left + top, right + top),
  [
    #text(size: 12pt, weight: "bold")[#d.header.organization]
    #if d.header.at("subtitle", default: none) != none [
      #v(-4pt)
      #text(size: 10pt, fill: gray)[#d.header.subtitle]
    ]
  ],
  align(right)[
    #set text(size: 9pt)
    #for f in d.header.at("fields", default: ()) [
      #text(fill: gray)[#f.label:] #h(4pt) #f.value \
    ]
  ],
)
#v(6pt)
#align(center)[#text(size: 17pt, weight: "bold")[#upper(d.title)]]
#v(10pt)

// ---- Articles --------------------------------------------------------
#let article-counter = counter("article")
#for a in d.articles {
  article-counter.step()
  block(breakable: true)[
    #context text(weight: "bold")[
      Article #numbering("I.", article-counter.get().first())
      #h(6pt) #a.heading
    ]
    #v(2pt)
    #if a.at("intro", default: none) != none [
      #pad(left: 1.2cm)[#a.intro]
      #v(2pt)
    ]
    #for f in a.at("fields", default: ()) [
      #pad(left: 1.2cm)[
        #grid(
          columns: (3.6cm, 1fr),
          text(fill: gray)[#f.label], strong(f.value),
        )
      ]
    ]
    #for p in a.at("body", default: ()) [
      #pad(left: 1.2cm)[#p]
      #v(2pt)
    ]
  ]
  v(8pt)
}

// ---- Execution -------------------------------------------------------
#if d.at("execution", default: none) != none [
  #v(6pt)
  #line(length: 100%, stroke: 0.4pt + gray)
  #v(4pt)
  #d.execution.statement
  #v(6pt)
  *#d.execution.signatory*
]
