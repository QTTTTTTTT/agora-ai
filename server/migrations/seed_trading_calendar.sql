-- seed_trading_calendar.sql
--
-- Seed trading_calendar with rows for a_share / us_equity / crypto so
-- marketstatus.Engine.evalCalendar actually fires. Without seed rows,
-- GetCalendarDay returns (nil, nil) and the engine short-circuits to
-- DecisionAllow, which is why a 9:01 CST A-share sell was happily
-- matched on 2026-06-03.
--
-- Window: today − 60 days through today + 30 days. Re-runnable: the
-- table has UNIQUE(market, trading_date) so re-running upserts each
-- row idempotently. The Chinese & US 2026 closures are inlined to
-- avoid pulling in marketcalendar at SQL-seed time.

-- ---------- a_share (SSE/SZSE, Asia/Shanghai, 09:30-15:00) ----------
INSERT INTO trading_calendar (market, trading_date, is_open, open_local, close_local, market_tz, half_day, note)
SELECT
    'a_share' AS market,
    d::date   AS trading_date,
    CASE
      WHEN EXTRACT(ISODOW FROM d) IN (6, 7) THEN FALSE   -- weekend
      WHEN d::date IN (
        '2026-01-01','2026-01-02',
        '2026-02-16','2026-02-17','2026-02-18','2026-02-19','2026-02-20','2026-02-23',
        '2026-04-06',
        '2026-05-01','2026-05-04','2026-05-05',
        '2026-06-19',
        '2026-09-25',
        '2026-10-01','2026-10-02','2026-10-05','2026-10-06','2026-10-07'
      ) THEN FALSE
      ELSE TRUE
    END        AS is_open,
    '09:30:00' AS open_local,
    '15:00:00' AS close_local,
    'Asia/Shanghai' AS market_tz,
    FALSE      AS half_day,
    'seed: a_share regular session' AS note
FROM generate_series(
    (CURRENT_DATE - INTERVAL '60 days')::date,
    (CURRENT_DATE + INTERVAL '30 days')::date,
    INTERVAL '1 day'
) AS d
ON CONFLICT (market, trading_date) DO UPDATE
  SET is_open = EXCLUDED.is_open,
      open_local = EXCLUDED.open_local,
      close_local = EXCLUDED.close_local,
      market_tz = EXCLUDED.market_tz,
      half_day = EXCLUDED.half_day,
      note = EXCLUDED.note;

-- ---------- us_equity (NASDAQ/NYSE, America/New_York, 09:30-16:00) ----------
INSERT INTO trading_calendar (market, trading_date, is_open, open_local, close_local, market_tz, half_day, note)
SELECT
    'us_equity' AS market,
    d::date     AS trading_date,
    CASE
      WHEN EXTRACT(ISODOW FROM d) IN (6, 7) THEN FALSE
      WHEN d::date IN (
        '2026-01-01','2026-01-19','2026-02-16','2026-04-03',
        '2026-05-25','2026-06-19','2026-07-03',
        '2026-09-07','2026-11-26','2026-12-25'
      ) THEN FALSE
      ELSE TRUE
    END         AS is_open,
    '09:30:00'  AS open_local,
    -- Black Friday + Christmas Eve half-day = 13:00 close.
    CASE
      WHEN d::date IN ('2026-11-27','2026-12-24') THEN '13:00:00'
      ELSE '16:00:00'
    END         AS close_local,
    'America/New_York' AS market_tz,
    CASE
      WHEN d::date IN ('2026-11-27','2026-12-24') THEN TRUE
      ELSE FALSE
    END         AS half_day,
    'seed: us_equity regular session' AS note
FROM generate_series(
    (CURRENT_DATE - INTERVAL '60 days')::date,
    (CURRENT_DATE + INTERVAL '30 days')::date,
    INTERVAL '1 day'
) AS d
ON CONFLICT (market, trading_date) DO UPDATE
  SET is_open = EXCLUDED.is_open,
      open_local = EXCLUDED.open_local,
      close_local = EXCLUDED.close_local,
      market_tz = EXCLUDED.market_tz,
      half_day = EXCLUDED.half_day,
      note = EXCLUDED.note;

-- ---------- crypto (24x7) ----------
INSERT INTO trading_calendar (market, trading_date, is_open, open_local, close_local, market_tz, half_day, note)
SELECT
    'crypto'    AS market,
    d::date     AS trading_date,
    TRUE        AS is_open,
    '00:00:00'  AS open_local,
    '23:59:59'  AS close_local,
    'UTC'       AS market_tz,
    FALSE       AS half_day,
    'seed: crypto 24x7' AS note
FROM generate_series(
    (CURRENT_DATE - INTERVAL '60 days')::date,
    (CURRENT_DATE + INTERVAL '30 days')::date,
    INTERVAL '1 day'
) AS d
ON CONFLICT (market, trading_date) DO UPDATE
  SET is_open = EXCLUDED.is_open,
      open_local = EXCLUDED.open_local,
      close_local = EXCLUDED.close_local,
      market_tz = EXCLUDED.market_tz,
      half_day = EXCLUDED.half_day,
      note = EXCLUDED.note;
