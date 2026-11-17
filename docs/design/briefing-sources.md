# 极客早报来源核查

核查日期：2026-11-17。目的：确认 NixOS 栏目的时效和内容性质，避免把旧 RSS、普通配置求助或长期 PR 催审队列当作每日技术新闻。下列日期是核查时公开入口的观测结果，不表示网站以后不会再更新。

| 来源 | 实测与用途 | 本轮决定 |
| --- | --- | --- |
| [NixOS 官方公告 RSS](https://nixos.org/blog/announcements-rss.xml) | 可读取，最新 pubDate 为 2026-07-10；权威但低频 | 保留，过去 24 小时无更新时允许为空；给新官方公告保留候选机会 |
| [Discourse 公告](https://discourse.nixos.org/c/announcements/8) | 版本、项目发布与社区公告，亦有非技术内容 | 保留，模型按技术价值筛选；旧公告仅依据 24 小时内可读新回复 |
| [Discourse Development](https://discourse.nixos.org/c/dev/14) | 活跃，兼有具体开发设计和长期 PR 催审队列 | 仅选过去 24 小时新发主题，排除旧队列，不把最后回复时间当发布时间 |
| [Discourse Guides](https://discourse.nixos.org/c/howto/15) | 完整教程，发布频率较低 | 仅选 24 小时新发教程，不以旧教程的新评论充当新闻 |
| [Lobsters Nix 标签](https://lobste.rs/t/nix) | 公开 RSS 可用，偏技术文章与项目发现；样本最新提交约为 2026-11-16 | 可作后续文章发现补充；提交时间不等于原文发布日期，不能承诺每日五条 |
| [Lix 官方新闻](https://lix.systems/blog/) | 可读的版本与安全公告，公开列表最新为 2026-10-13 | 有价值的低频候选；若后续接入需标明 Lix，不冒充上游 Nix/NixOS 发布 |
| [NixOS Weekly](https://weekly.nixos.org/) | 公开列表最新为 2021-09-06 | 不作为 24 小时日报源 |
| [Planet NixOS](https://planet.nixos.org/) | 聚合页最新显示 2022-02-03 | 不作为当前日报源 |
| [nixpkgs.news RSS](https://nixpkgs.news/rss.xml) | HTTP 200，但最新 pubDate 为 2024-07-07 | 不以“接口可用”代替内容时效检查，不接入 |
| [Tweag Nix RSS](https://www.tweag.io/rss-nix.xml) | 此地址返回 404，主博客仍可读 | 不接入失效地址；不据此判断整个博客停更 |
| [NixOS/nix GitHub Releases](https://github.com/NixOS/nix/releases) | releases API 返回空列表，网页亦没有发布条目 | 不把不存在的 release 流设为每日主源 |

当前保留四大栏目，NixOS 排最后。HN 偏全球技术讨论，GitHub 栏目明确是开源发现，V2EX 保留经过节点和内容过滤的国内技术话题。来源时效、全文可读性、重复度和有效条目数应分别评估，不能仅按热度或接口是否成功挑选。

每栏最多五条是容量上限；NixOS 小领域的真正新动态不保证每天五条。缺少合格材料时少发，不借助生活热榜、求助报错、过期周报或陈旧教程补足。新增来源应先验证真实响应、发布时间字段与材料类型，再纳入一次批量编辑流程。
