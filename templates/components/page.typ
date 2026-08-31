// Shared page chrome: data-driven header/footer (canopy's components/
// convention). Templates opt in with:
//
//   #import "components/page.typ": branded
//   #show: branded.with(d)
//
// and configure it through the payload's `page` object — editable in the
// designer's Page tab, defaultable in <template>.defaults.json, and
// overridable per request like any other key:
//
//   "page": {
//     "paper": "a4",            // "a4" | "us-letter" | "legal" | ...
//     "margin_cm": 2,
//     "header_left": "",        // any of the four corners may be empty
//     "header_right": "",
//     "footer_left": "",
//     "footer_right": "",
//     "page_numbers": true      // "Page N of M" at the footer's right
//   }
//
// Every key is optional: a missing `page` object renders a plain page.

/// Apply branded page setup from `d.page`, then lay out the body.
#let branded(d, body) = {
  let cfg = d.at("page", default: (:))
  let hl = cfg.at("header_left", default: "")
  let hr = cfg.at("header_right", default: "")
  let fl = cfg.at("footer_left", default: "")
  let fr = cfg.at("footer_right", default: "")
  let nums = cfg.at("page_numbers", default: true)
  let m = cfg.at("margin_cm", default: 2)

  set page(
    paper: cfg.at("paper", default: "a4"),
    margin: (x: m * 1cm, y: m * 1cm),
    header: if hl != "" or hr != "" [
      #set text(size: 8.5pt, fill: gray)
      #hl
      #h(1fr)
      #hr
      #v(-4pt)
      #line(length: 100%, stroke: 0.4pt + gray.lighten(40%))
    ],
    footer: if fl != "" or fr != "" or nums [
      #set text(size: 8.5pt, fill: gray)
      #line(length: 100%, stroke: 0.4pt + gray.lighten(40%))
      #v(-4pt)
      #fl
      #h(1fr)
      #fr
      #if nums [
        #if fr != "" [#h(10pt)]
        Page #context counter(page).display() of #context counter(page).final().first()
      ]
    ],
  )
  body
}
