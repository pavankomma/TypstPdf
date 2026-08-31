// Report template. Input schema: see examples/report.json.
// The service drops the request body next to this file as data.json.

#let d = json("data.json")

#set page(paper: "a4", margin: (x: 2.2cm, y: 2cm), footer: context [
  #set text(size: 8pt, fill: gray)
  #d.title
  #h(1fr)
  Page #counter(page).display() of #counter(page).final().first()
])
#set text(size: 10.5pt)
#set par(justify: true)

// ---- Title block -----------------------------------------------------
#align(center)[
  #text(size: 20pt, weight: "bold")[#d.title]
  #if d.at("subtitle", default: none) != none [
    #v(2pt)
    #text(size: 12pt, fill: gray)[#d.subtitle]
  ]
  #v(4pt)
  #text(size: 9pt, fill: gray)[#d.author — #d.date]
]
#v(6pt)
#line(length: 100%, stroke: 0.5pt + gray)
#v(6pt)

// ---- Key metrics (optional) ------------------------------------------
#if d.at("metrics", default: ()).len() > 0 {
  grid(
    columns: (1fr,) * d.metrics.len(),
    gutter: 8pt,
    ..d.metrics.map(m => rect(
      width: 100%,
      inset: 10pt,
      radius: 4pt,
      fill: rgb("#1a56db").lighten(92%),
      [
        #text(size: 8.5pt, fill: gray)[#upper(m.label)] \
        #text(size: 15pt, weight: "bold")[#m.value]
      ],
    )),
  )
  v(10pt)
}

// ---- Sections --------------------------------------------------------
#for s in d.sections [
  == #s.heading
  #for p in s.body [
    #p
  ]
]
