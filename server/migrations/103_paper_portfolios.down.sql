-- Migration: 103_paper_portfolios DOWN
--
-- Order: drop children first (FK cascade would do it but we drop
-- explicitly so the down-migration is intelligible).

DROP TABLE IF EXISTS paper_nav_history;
DROP TABLE IF EXISTS paper_holdings_snapshots;
DROP TABLE IF EXISTS paper_orders;
DROP TABLE IF EXISTS paper_portfolios;
