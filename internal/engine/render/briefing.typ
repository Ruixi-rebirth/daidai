#let data = json("content.json")
#let ink = rgb("#182333")
#let muted = rgb("#566578")
#let accents = (nixos: rgb("#2166b5"), hn: rgb("#ad4d0b"), github: rgb("#7053ad"), v2ex: rgb("#147568"))
#let labels = (nixos: "❄️  NixOS 生态", hn: "🔥  Hacker News 精选", github: "💻  GitHub 开源发现", v2ex: "💬  V2EX 技术话题")

#set page(width: 600pt, height: auto, margin: (x: 28pt, y: 24pt), fill: rgb("#f5f7fb"))
#set text(font: ("Twitter Color Emoji", "Source Han Sans SC"), size: 22pt, fill: ink, lang: "zh")
#set par(justify: false, leading: 0.48em, spacing: 0pt, linebreaks: "optimized")
#set block(spacing: 0pt)

#let masthead() = [
  #grid(columns: (1fr, auto), align: (left, right), column-gutter: 8pt,
    text(size: 15pt, weight: "bold", fill: muted)[DAIDAI / 呆呆],
    text(size: 14pt, fill: muted)[每日精选],
  )
  #v(9pt)
  #text(size: 36pt, weight: "bold")[极客早报]
  #v(5pt)
  #text(size: 16pt, fill: muted)[#data.date · #data.weekday · 截止 #data.cutoff]
  #if data.basic [#v(5pt) #text(size: 16pt, fill: rgb("#885a2b"))[本期为基础速览]]
  #if data.source_note != "" [#v(5pt) #text(size: 16pt, fill: muted)[#data.source_note]]
  #v(13pt)
  #line(length: 100%, stroke: 0.7pt + rgb("#d6dfea"))
  #if data.leads.len() > 0 [
    #v(12pt)
    #block(width: 100%, fill: rgb("#eaf0fa"), radius: 9pt, inset: 12pt)[
      #text(size: 16pt, weight: "bold", fill: rgb("#31567e"))[先看这两条]
      #v(5pt)
      #for (index, number) in data.leads.enumerate() {
        if index > 0 { v(3pt) }
        let item = data.items.find(item => item.number == number)
        text(size: 18pt)[#strong[#item.number] #item.title]
      }
    ]
  ]
]

#let section(source) = [
  #v(15pt)
  #text(size: 18pt, weight: "bold", fill: accents.at(source))[#labels.at(source)]
  #v(8pt)
]

#let story(item) = [
  #block(width: 100%, fill: white, radius: 10pt, stroke: 0.5pt + rgb("#dfe5ed"), inset: 16pt, breakable: false)[
    #grid(columns: (34pt, 1fr), column-gutter: 9pt, align: (left, top),
      text(font: "Maple Mono", size: 22pt, weight: "bold", fill: accents.at(item.source))[#item.number],
      text(size: 26pt, weight: "bold", lang: "en", hyphenate: true)[#item.title],
    )
    #if item.summary != "" [
      #v(9pt)
      #text(size: 22pt)[#item.summary]
    ]
    #v(10pt)
    #text(size: 16pt, fill: muted)[#item.meta]
    #if item.summary == "" and not data.basic [
      #v(5pt)
      #text(size: 15pt, fill: rgb("#885a2b"))[#if item.evidence_kind == "title_only" [标题速览 · 未获取正文] else [本条暂无简短摘要]]
    ]
  ]
  #v(9pt)
]

#masthead()
#for (index, item) in data.items.enumerate() {
  if index == 0 or data.items.at(index - 1).source != item.source {
    section(item.source)
  }
  story(item)
}
#v(6pt)
#line(length: 100%, stroke: 0.5pt + rgb("#d6dfea"))
#v(9pt)
#text(size: 15pt, fill: muted)[每日精选 · #data.items.len() 条]
