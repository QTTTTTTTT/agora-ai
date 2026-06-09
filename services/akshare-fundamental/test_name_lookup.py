"""Unit tests for the sidecar's name-lookup + period-row helpers.

We mock urllib.request.urlopen for the network-hitting tests so the
CI doesn't depend on sina being reachable. Pure logic
(``_exchange_prefix``, ``_latest_annual_row``, ``_latest_period_row``)
is tested directly.

Run with:
    docker compose exec -T akshare-fundamental python -m unittest test_name_lookup -v
"""

from __future__ import annotations

import io
import unittest
from unittest import mock

import server  # the Flask app module under test


class TestExchangePrefix(unittest.TestCase):
    """``_exchange_prefix`` is the pure routing logic."""

    def test_shanghai_main_and_star(self):
        self.assertEqual(server._exchange_prefix("600519"), "sh")  # 茅台
        self.assertEqual(server._exchange_prefix("688205"), "sh")  # 德科立 (STAR)
        self.assertEqual(server._exchange_prefix("601318"), "sh")  # 平安

    def test_shenzhen_main_and_chinext(self):
        self.assertEqual(server._exchange_prefix("000001"), "sz")  # 平安银行
        self.assertEqual(server._exchange_prefix("300750"), "sz")  # 宁德
        self.assertEqual(server._exchange_prefix("002594"), "sz")  # 比亚迪

    def test_beijing_exchange(self):
        # Beijing Stock Exchange uses 4xxxxx, 8xxxxx and 9xxxxx
        # prefixes — most public examples are 8xxxxx (e.g. 832000,
        # 873169).
        self.assertEqual(server._exchange_prefix("832000"), "bj")
        self.assertEqual(server._exchange_prefix("430090"), "bj")
        self.assertEqual(server._exchange_prefix("920000"), "bj")

    def test_unknown_codes(self):
        # Wrong length, non-digit, or a digit head we don't route:
        self.assertIsNone(server._exchange_prefix(""))
        self.assertIsNone(server._exchange_prefix("12345"))    # 5 digits
        self.assertIsNone(server._exchange_prefix("1234567"))  # 7 digits
        self.assertIsNone(server._exchange_prefix("AAPL"))     # not numeric
        self.assertIsNone(server._exchange_prefix("100000"))   # leading 1 — unallocated
        self.assertIsNone(server._exchange_prefix("500000"))   # leading 5 — funds, not equity


class TestLookupName(unittest.TestCase):
    """``lookup_name`` wraps the sina hqjs HTTP call. We mock the
    HTTP layer so the test is hermetic."""

    def setUp(self):
        # Each test starts with a clean cache so cache state from
        # one test doesn't leak into another.
        with server._name_cache_lock:
            server._name_cache.clear()

    def _mock_response(self, text: str):
        """Wrap a string as a urllib response that decodes from
        gb18030 (sina's actual content-type)."""
        body = text.encode("gb18030")
        resp = mock.MagicMock()
        # urlopen returns a context manager; .read() returns bytes.
        resp.__enter__.return_value.read.return_value = body
        return resp

    def test_parses_sina_quote_line(self):
        sina_body = (
            'var hq_str_sh688205="德科立,209.100,226.700,213.050,217.000,'
            '209.100,213.080,213.240,4159672,890452942.000";\n'
        )
        with mock.patch("urllib.request.urlopen", return_value=self._mock_response(sina_body)):
            self.assertEqual(server.lookup_name("688205"), "德科立")

    def test_handles_unknown_symbol_format(self):
        # Bad symbol — we shouldn't even attempt the HTTP call.
        with mock.patch("urllib.request.urlopen") as urlopen:
            self.assertIsNone(server.lookup_name("garbage"))
            urlopen.assert_not_called()

    def test_caches_successful_lookup(self):
        sina_body = 'var hq_str_sh600519="贵州茅台,1800.00";\n'
        with mock.patch(
            "urllib.request.urlopen", return_value=self._mock_response(sina_body)
        ) as urlopen:
            self.assertEqual(server.lookup_name("600519"), "贵州茅台")
            self.assertEqual(server.lookup_name("600519"), "贵州茅台")
            # Second call should be served from cache:
            self.assertEqual(urlopen.call_count, 1)

    def test_caches_empty_result(self):
        # Sina sometimes returns the var with an empty name for
        # delisted / nonexistent codes. We cache that as None so we
        # don't hammer the upstream every request.
        sina_body = 'var hq_str_sh999999="";\n'
        with mock.patch(
            "urllib.request.urlopen", return_value=self._mock_response(sina_body)
        ) as urlopen:
            self.assertIsNone(server.lookup_name("999999"))
            self.assertIsNone(server.lookup_name("999999"))
            self.assertEqual(urlopen.call_count, 1)

    def test_network_error_returns_none(self):
        import urllib.error
        with mock.patch("urllib.request.urlopen", side_effect=urllib.error.URLError("net down")):
            self.assertIsNone(server.lookup_name("688205"))

    def test_unparseable_response_returns_none(self):
        # Sina is reachable but didn't return the expected shape
        # (e.g. CDN error page, html, empty body).
        with mock.patch("urllib.request.urlopen", return_value=self._mock_response("<html>500</html>")):
            self.assertIsNone(server.lookup_name("688205"))


class TestPeriodRowSelectors(unittest.TestCase):
    """``_latest_annual_row`` and ``_latest_period_row`` are the
    pure logic gating which fiscal period each metric is drawn from.

    These tests pin the contract that a stock that has reported a
    QUARTER past its latest ANNUAL must surface BOTH periods —
    that's the bug behind Wood's stale -28.77% verdict on 688205:
    the annual was 2025-12-31 (down) but a 2026-03-31 quarter was
    already on file showing a turnaround.
    """

    def _build_df(self, rows: list[dict], date_col: str = "日期"):
        """Build a tiny DataFrame from a list of dicts. We import
        pandas lazily because the test runner shouldn't crash if
        pandas is missing; the actual code path requires it."""
        import pandas as pd
        return pd.DataFrame(rows)

    def test_latest_annual_skips_interim(self):
        df = self._build_df([
            {"日期": "2024-12-31", "value": 1.0},
            {"日期": "2025-03-31", "value": 2.0},  # interim — should be skipped
            {"日期": "2025-06-30", "value": 3.0},  # interim
        ])
        row = server._latest_annual_row(df, "日期")
        self.assertIsNotNone(row)
        self.assertEqual(row["日期"], "2024-12-31")

    def test_latest_annual_falls_back_when_no_annual(self):
        # Newly-listed companies sometimes have only interim
        # periods on file. Fall back to the most recent interim
        # so the caller gets *something* rather than None.
        df = self._build_df([
            {"日期": "2025-03-31", "value": 1.0},
            {"日期": "2025-06-30", "value": 2.0},
        ])
        row = server._latest_annual_row(df, "日期")
        self.assertIsNotNone(row)
        self.assertEqual(row["日期"], "2025-06-30")

    def test_latest_period_picks_most_recent_regardless_of_type(self):
        # The 688205 scenario: 2025 annual is on file but a
        # 2026 Q1 has already been reported. _latest_period_row
        # MUST return the Q1, not the annual.
        df = self._build_df([
            {"日期": "2024-12-31", "value": 1.0},
            {"日期": "2025-12-31", "value": 2.0},
            {"日期": "2026-03-31", "value": 3.0},
        ])
        row = server._latest_period_row(df, "日期")
        self.assertIsNotNone(row)
        self.assertEqual(row["日期"], "2026-03-31")

    def test_period_selectors_diverge_when_quarter_fresher_than_annual(self):
        # The whole point of having two helpers: when a quarter
        # is fresher than the latest annual, the two MUST disagree
        # so the sidecar knows to ship both _annual_ and _latest_
        # fields.
        df = self._build_df([
            {"日期": "2025-12-31", "value": "annual"},
            {"日期": "2026-03-31", "value": "q1"},
        ])
        annual = server._latest_annual_row(df, "日期")
        latest = server._latest_period_row(df, "日期")
        self.assertNotEqual(annual["日期"], latest["日期"])
        self.assertEqual(annual["value"], "annual")
        self.assertEqual(latest["value"], "q1")

    def test_period_selectors_agree_when_latest_is_annual(self):
        # Stocks that haven't reported a quarter past the annual:
        # the two selectors agree, and the sidecar should NOT ship
        # redundant _latest_ fields.
        df = self._build_df([
            {"日期": "2024-12-31", "value": "old_annual"},
            {"日期": "2025-12-31", "value": "new_annual"},
        ])
        annual = server._latest_annual_row(df, "日期")
        latest = server._latest_period_row(df, "日期")
        self.assertEqual(annual["日期"], latest["日期"])


class TestNormalizeISODate(unittest.TestCase):
    """``_normalize_iso_date`` is what bridges cninfo's date column
    (which oscillates between datetime objects, ``YYYY-MM-DD`` strings,
    and ``YYYY/MM/DD`` slashes depending on akshare version) and the
    canonical ``YYYY-MM-DD`` shape the Go side expects.
    """

    def test_dash_string_passes_through(self):
        self.assertEqual(server._normalize_iso_date("2022-08-09"), "2022-08-09")

    def test_slash_string_normalises(self):
        self.assertEqual(server._normalize_iso_date("2022/08/09"), "2022-08-09")

    def test_compact_string_normalises(self):
        self.assertEqual(server._normalize_iso_date("20220809"), "2022-08-09")

    def test_datetime_object(self):
        from datetime import datetime as dt
        self.assertEqual(server._normalize_iso_date(dt(2022, 8, 9)), "2022-08-09")

    def test_pandas_timestamp(self):
        import pandas as pd
        self.assertEqual(server._normalize_iso_date(pd.Timestamp("2022-08-09")), "2022-08-09")

    def test_garbage_returns_empty(self):
        self.assertEqual(server._normalize_iso_date(""), "")
        self.assertEqual(server._normalize_iso_date(None), "")
        self.assertEqual(server._normalize_iso_date("--"), "")
        self.assertEqual(server._normalize_iso_date(False), "")
        self.assertEqual(server._normalize_iso_date("nan"), "")


class TestYearsSince(unittest.TestCase):
    """``_years_since`` is the tenure calculator used to emit
    ``listing_years``. The number is shipped to the LLM so the persona
    can stop penalising sub-10-year listings as 'history.10yr
    data_unavailable' (rule 7 in master_agent.go)."""

    def test_decimal_year_precision(self):
        # 2022-08-09 → 2024-08-09 = exactly 2.0 years (within 1 day for
        # leap-year rounding; we accept ±0.01y).
        from datetime import datetime as dt
        y = server._years_since("2022-08-09", asof=dt(2024, 8, 9))
        self.assertIsNotNone(y)
        self.assertAlmostEqual(y, 2.0, places=1)

    def test_fresh_ipo_yields_small_fraction(self):
        # IPO 30 days ago should be ~0.08y, not 0.
        from datetime import datetime as dt, timedelta
        listed = dt(2025, 1, 1)
        asof = listed + timedelta(days=30)
        y = server._years_since(listed.strftime("%Y-%m-%d"), asof=asof)
        self.assertIsNotNone(y)
        self.assertGreater(y, 0.0)
        self.assertLess(y, 0.2)

    def test_empty_input_returns_none(self):
        self.assertIsNone(server._years_since(""))
        self.assertIsNone(server._years_since(None))  # type: ignore[arg-type]

    def test_bad_format_returns_none(self):
        self.assertIsNone(server._years_since("not-a-date"))

    def test_future_listing_returns_none(self):
        # Defensive: if the source mis-reports a date in the future
        # (e.g. an IPO in registration), don't emit a negative tenure.
        from datetime import datetime as dt
        y = server._years_since("2099-01-01", asof=dt(2026, 1, 1))
        self.assertIsNone(y)


class TestCandidateYjbbPeriods(unittest.TestCase):
    """``_candidate_yjbb_periods`` decides which 业绩快报 periods to
    probe given today's date. Pinning the ordering matters because
    the first hit wins — a wrong order means a stock that filed Q1
    but not H1 yet would still get its (stale) Q4 numbers."""

    def _periods(self, y, m, d):
        from datetime import datetime
        return server._candidate_yjbb_periods(datetime(y, m, d))

    def test_early_jan_only_prior_year(self):
        # Jan 5: nothing from current year has been filed yet. Latest
        # plausibly-on-file period is prior year FY (1231).
        p = self._periods(2026, 1, 5)
        self.assertEqual(p[0], "20251231")
        # No 20260101+ in the list (they're in the future).
        for x in p:
            self.assertLess(x, "20260105")

    def test_may_after_q1_filing(self):
        # May 1: Q1 filings (deadline Apr 30) should be on file.
        p = self._periods(2026, 5, 1)
        self.assertEqual(p[0], "20260331")

    def test_july_h1_still_grace(self):
        # Jul 15: H1 (filing deadline Aug 31) NOT yet on file in bulk,
        # so the most recent should still be Q1.
        p = self._periods(2026, 7, 15)
        self.assertEqual(p[0], "20260331")

    def test_september_after_h1(self):
        # Sep 5: H1 should be the freshest.
        p = self._periods(2026, 9, 5)
        self.assertEqual(p[0], "20260630")

    def test_november_after_q3(self):
        # Nov 5: Q3 (deadline Oct 31) should be the freshest.
        p = self._periods(2026, 11, 5)
        self.assertEqual(p[0], "20260930")

    def test_no_future_periods_emitted(self):
        # Any candidate date must be strictly <= today; we should
        # never probe a period that hasn't ended yet.
        p = self._periods(2026, 6, 8)
        for x in p:
            self.assertLess(x, "20260608", f"future period leaked: {x}")


class TestLatestYjbbEm(unittest.TestCase):
    """``_latest_yjbb_em`` is the citation-metadata extractor. It
    must (1) fetch only once per period (cache), (2) filter by
    symbol, (3) map the Chinese column names onto canonical
    English keys, and (4) fall back to a prior period when the
    most recent one doesn't contain the symbol.

    The fixture mirrors the verbatim shape Eastmoney returned for
    688205 2026-Q1 (see 'Smoking gun' in prior conversation):
        营业总收入-营业总收入   = 254,444,250
        营业总收入-同比增长     = 27.9697627449
        营业总收入-季度环比增长 = -9.6262
        净利润-同比增长         = 35.08
        最新公告日期            = 2026-04-28
    """

    def setUp(self):
        with server._yjbb_cache_lock:
            server._yjbb_cache.clear()

    def _yjbb_df(self, symbol="688205", **overrides):
        import pandas as pd
        row = {
            "股票代码": symbol,
            "股票简称": "德科立",
            "营业总收入-营业总收入": 254_444_250.0,
            "营业总收入-同比增长": 27.9697627449,
            "营业总收入-季度环比增长": -9.6262,
            "净利润-净利润": 19_639_010.0,
            "净利润-同比增长": 35.08,
            "净利润-季度环比增长": -37.5526,
            "销售毛利率": 25.7339594037,
            "最新公告日期": "2026-04-28",
        }
        row.update(overrides)
        return pd.DataFrame([row, {"股票代码": "999998", "股票简称": "noise"}])

    def test_extracts_all_fields(self):
        from datetime import datetime
        with mock.patch("server.ak.stock_yjbb_em", return_value=self._yjbb_df()):
            out = server._latest_yjbb_em("688205", ref=datetime(2026, 6, 8))
        self.assertEqual(out["latest_source"], "eastmoney_yjbb")
        self.assertEqual(out["latest_announce_date"], "2026-04-28")
        self.assertEqual(out["latest_revenue"], 254_444_250.0)
        self.assertEqual(out["latest_net_income"], 19_639_010.0)
        # Percent fields divided by 100:
        self.assertAlmostEqual(out["revenue_growth_latest"], 0.279697627449, places=6)
        self.assertAlmostEqual(out["earnings_growth_latest"], 0.3508, places=4)
        self.assertAlmostEqual(out["latest_revenue_qoq"], -0.096262, places=6)
        self.assertAlmostEqual(out["latest_net_income_qoq"], -0.375526, places=6)
        self.assertAlmostEqual(out["gross_margin_latest"], 0.257339594037, places=6)

    def test_caches_period_snapshot(self):
        # _latest_yjbb_em for two different symbols against the
        # same period should hit ak.stock_yjbb_em ONCE, not twice.
        from datetime import datetime
        import pandas as pd
        full = pd.DataFrame([
            {**self._yjbb_df().iloc[0].to_dict(), "股票代码": "688205"},
            {**self._yjbb_df().iloc[0].to_dict(), "股票代码": "300750", "股票简称": "宁德时代"},
        ])
        with mock.patch("server.ak.stock_yjbb_em", return_value=full) as fn:
            out1 = server._latest_yjbb_em("688205", ref=datetime(2026, 6, 8))
            out2 = server._latest_yjbb_em("300750", ref=datetime(2026, 6, 8))
            self.assertTrue(out1)
            self.assertTrue(out2)
            self.assertEqual(fn.call_count, 1)

    def test_falls_back_to_prior_period_when_symbol_absent(self):
        # Stock didn't make the 2026Q1 snapshot but DID file 2025-Q4
        # (newly-listed or stale-filing scenario). _latest_yjbb_em
        # must walk back periods until it hits one that contains
        # the symbol.
        import pandas as pd
        from datetime import datetime
        q1_empty = pd.DataFrame([{"股票代码": "999998", "股票简称": "noise"}])
        q4_has_target = self._yjbb_df(symbol="688205")
        call_log = []

        def fake_yjbb(date):
            call_log.append(date)
            if date == "20260331":
                return q1_empty
            if date == "20251231":
                return q4_has_target
            return pd.DataFrame()

        with mock.patch("server.ak.stock_yjbb_em", side_effect=fake_yjbb):
            out = server._latest_yjbb_em("688205", ref=datetime(2026, 6, 8))
        self.assertTrue(out, "expected fallback to find prior-period filing")
        self.assertEqual(out["latest_revenue"], 254_444_250.0)
        # Must have tried 2026Q1 BEFORE 2025Q4:
        self.assertEqual(call_log[0], "20260331")
        self.assertIn("20251231", call_log)

    def test_upstream_failure_returns_empty(self):
        from datetime import datetime
        with mock.patch("server.ak.stock_yjbb_em", side_effect=ConnectionError("eastmoney down")):
            out = server._latest_yjbb_em("688205", ref=datetime(2026, 6, 8))
        self.assertEqual(out, {})

    def test_upstream_failure_not_cached(self):
        # Eastmoney blip on one period must NOT poison the cache —
        # next request for any symbol on that period should retry.
        from datetime import datetime
        with mock.patch("server.ak.stock_yjbb_em", side_effect=ConnectionError("blip")) as fn:
            server._latest_yjbb_em("688205", ref=datetime(2026, 6, 8))
            server._latest_yjbb_em("688205", ref=datetime(2026, 6, 8))
            # Each candidate period tried twice (one per call) → at
            # least one period retried.
            self.assertGreater(fn.call_count, len(server._candidate_yjbb_periods(datetime(2026, 6, 8))))

    def test_empty_symbol_returns_empty(self):
        # Defensive: don't even attempt the heavy upstream call
        # for a blank symbol.
        with mock.patch("server.ak.stock_yjbb_em") as fn:
            self.assertEqual(server._latest_yjbb_em(""), {})
            self.assertEqual(server._latest_yjbb_em("   "), {})
            fn.assert_not_called()


class TestLookupListingDate(unittest.TestCase):
    """``lookup_listing_date`` wraps ``ak.stock_profile_cninfo``. We
    patch the akshare call so the test is hermetic and doesn't depend
    on cninfo being reachable in CI.
    """

    def setUp(self):
        with server._profile_cache_lock:
            server._profile_cache.clear()

    def _profile_df(self, listing_date_val):
        """Build a single-row cninfo-shaped DataFrame with whatever
        we want in the 上市日期 column."""
        import pandas as pd
        return pd.DataFrame([{"公司名称": "无锡市德科立", "上市日期": listing_date_val}])

    def test_parses_dash_string(self):
        with mock.patch("server.ak.stock_profile_cninfo", return_value=self._profile_df("2022-08-09")):
            self.assertEqual(server.lookup_listing_date("688205"), "2022-08-09")

    def test_parses_pandas_timestamp(self):
        import pandas as pd
        with mock.patch("server.ak.stock_profile_cninfo",
                        return_value=self._profile_df(pd.Timestamp("2022-08-09"))):
            self.assertEqual(server.lookup_listing_date("688205"), "2022-08-09")

    def test_caches_successful_lookup(self):
        # Listing date NEVER changes; we should hit cninfo once and
        # serve the rest from cache for the lifetime of the process.
        with mock.patch("server.ak.stock_profile_cninfo",
                        return_value=self._profile_df("2022-08-09")) as fn:
            self.assertEqual(server.lookup_listing_date("688205"), "2022-08-09")
            self.assertEqual(server.lookup_listing_date("688205"), "2022-08-09")
            self.assertEqual(server.lookup_listing_date("688205"), "2022-08-09")
            self.assertEqual(fn.call_count, 1)

    def test_caches_empty_result(self):
        # cninfo answered but the row had no parseable date. Cache
        # the negative result so we don't re-hit on every request.
        with mock.patch("server.ak.stock_profile_cninfo",
                        return_value=self._profile_df("--")) as fn:
            self.assertIsNone(server.lookup_listing_date("999999"))
            self.assertIsNone(server.lookup_listing_date("999999"))
            self.assertEqual(fn.call_count, 1)

    def test_network_error_returns_none_and_does_not_cache(self):
        # Transient failures must NOT poison the cache — the next
        # request should retry the upstream.
        with mock.patch("server.ak.stock_profile_cninfo",
                        side_effect=ConnectionError("cninfo down")) as fn:
            self.assertIsNone(server.lookup_listing_date("688205"))
            self.assertIsNone(server.lookup_listing_date("688205"))
            self.assertEqual(fn.call_count, 2)

    def test_empty_dataframe_returns_none(self):
        import pandas as pd
        with mock.patch("server.ak.stock_profile_cninfo", return_value=pd.DataFrame()):
            self.assertIsNone(server.lookup_listing_date("688205"))


if __name__ == "__main__":
    unittest.main()
