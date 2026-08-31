// Formal letter template. Input schema: see examples/letter.json.
// The service drops the request body next to this file as data.json.

#let d = json("data.json")

#set page(paper: "a4", margin: (x: 2.4cm, y: 2.4cm))
#set text(size: 11pt)
#set par(justify: true)

// ---- Sender ----------------------------------------------------------
#align(right)[
  *#d.sender.name* \
  #d.sender.address.join([ \ ])
]
#v(14pt)
#d.date
#v(14pt)

// ---- Recipient -------------------------------------------------------
*#d.recipient.name* \
#d.recipient.address.join([ \ ])
#v(16pt)

// ---- Subject & body --------------------------------------------------
#if d.at("subject", default: none) != none [
  *Subject: #d.subject*
  #v(8pt)
]

#d.salutation

#for p in d.body [
  #p
]

#v(16pt)
#d.closing \
#v(24pt)
*#d.sender.name*
