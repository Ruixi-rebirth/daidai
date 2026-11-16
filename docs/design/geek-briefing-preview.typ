// Design-only fixture. This template does not fetch or publish news.
#let data = json("geek-briefing-preview.json")
#let ink = rgb("#182333")
#let muted = rgb("#566578")
#let accents = (nixos: rgb("#2166b5"), hn: rgb("#ad4d0b"), github: rgb("#7053ad"), v2ex: rgb("#147568"))
#let labels = (nixos: "❄️  NixOS 生态", hn: "🔥  Hacker News 精选", github: "💻  GitHub 开源发现", v2ex: "💬  V2EX 技术话题")
#let pages = calc.max(..data.items.map(item => item.page))

#set page(width: 440pt, height: auto, margin: (x: 22pt, y: 20pt), fill: rgb("#f5f7fb"))
#set text(font: ("Twitter Color Emoji", "Source Han Sans SC"), size: 18pt, fill: ink, lang: "zh")
#set par(justify: false, leading: 0.48em, spacing: 0pt)
#set block(spacing: 0pt)

#let masthead(page) = [
  #grid(columns: (1fr, auto), align: (left, right), column-gutter: 8pt,
    text(size: 12pt, weight: "bold", fill: muted)[DAIDAI / 呆呆],
    text(size: 11pt, fill: rgb("#885a2b"))[设计样张 · 虚构资讯],
  )
  #v(9pt)
  #text(size: 29pt, weight: "bold")[极客早报]
  #v(5pt)
  #text(size: 13pt, fill: muted)[#data.date · #data.weekday · 截止 #data.cutoff]
  #v(13pt)
  #line(length: 100%, stroke: 0.7pt + rgb("#d6dfea"))
  #if page == 1 [
    #v(12pt)
    #block(width: 100%, fill: rgb("#eaf0fa"), radius: 9pt, inset: 12pt)[
      #text(size: 13pt, weight: "bold", fill: rgb("#31567e"))[先看这两条]
      #v(5pt)
      #for (index, number) in data.leads.enumerate() {
        if index > 0 { v(3pt) }
        let item = data.items.find(item => item.number == number)
        text(size: 15pt)[#strong[#item.number] #item.title]
      }
    ]
  ]
]

#let section(source, continued: false) = [
  #v(15pt)
  #text(size: 15pt, weight: "bold", fill: accents.at(source))[#labels.at(source)#if continued [ · 续]]
  #v(8pt)
]

#let story(item) = [
  #block(width: 100%, fill: white, radius: 10pt, stroke: 0.5pt + rgb("#dfe5ed"), inset: 14pt, breakable: false)[
    #grid(columns: (28pt, 1fr), column-gutter: 7pt, align: (left, top),
      text(font: "Maple Mono", size: 18pt, weight: "bold", fill: accents.at(item.source))[#item.number],
      text(size: 21pt, weight: "bold")[#item.title],
    )
    #if item.summary != "" [
      #v(9pt)
      #text(size: 18pt)[#item.summary]
    ]
    #v(10pt)
    #text(size: 13pt, fill: muted)[#item.meta]
    #if item.evidence_kind == "title_only" [
      #v(5pt)
      #text(size: 12pt, fill: rgb("#885a2b"))[标题速览 · 暂无正文摘要]
    ]
  ]
  #context metadata((number: item.number, planned_page: item.page, actual_page: counter(page).get().at(0)))
  #v(9pt)
]

#for page in range(1, pages + 1) {
  if page > 1 { pagebreak() }
  masthead(page)
  let items = data.items.filter(item => item.page == page)
  for (index, item) in items.enumerate() {
    if index == 0 or items.at(index - 1).source != item.source {
      let continued = page > 1 and data.items.any(other => other.source == item.source and other.page < page)
      section(item.source, continued: continued)
    }
    story(item)
  }
  v(6pt)
  line(length: 100%, stroke: 0.5pt + rgb("#d6dfea"))
  v(9pt)
  grid(columns: (1fr, auto), align: (left, right),
    text(size: 12pt, fill: muted)[每日精选 · #data.items.len() 条],
    text(font: "Maple Mono", size: 13pt, weight: "bold", fill: muted)[[#page/#pages]],
  )
}
