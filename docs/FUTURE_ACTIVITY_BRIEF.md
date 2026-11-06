# Later brief: station and team activity

**Status:** deferred. This is context, not an active implementation PRD. The CCTV MVP in [PRD.md](PRD.md) must work first.

The owner ultimately wants to review whether work stations in the pizza shop show sustained **visible** activity. The first chosen signal is a combination of person presence in a station zone and movement within or between zones. Reporting starts at station/team level, not named employees. Possible areas include order counter/POS, pizza prep, oven/cut/box, pickup, and cleaning/restocking where cameras actually see them.

A future PRD should specify zone polygons from real camera views, operating hours, sampling intervals, review clips, model evaluation, privacy/notice practices, compute cost, and how to treat customer traffic and legitimate stationary work. Missing video is **unknown**, not inactivity. The metric should be reviewable evidence rather than an automatic productivity verdict.

No AI model, detector, or analytics service belongs in the MVP recording path.
