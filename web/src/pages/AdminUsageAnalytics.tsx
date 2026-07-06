import React from "react";
import { Link } from "react-router-dom";
import AdminUsageAnalyticsSection from "../components/AdminUsageAnalyticsSection";

export default function AdminUsageAnalytics() {
  return (
    <div className="mx-auto max-w-7xl space-y-5 px-4 py-8">
      <header className="rounded-2xl border border-gray-200 bg-white px-5 py-5 shadow-sm">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <h1 className="text-2xl font-bold text-gray-900">大师团与埋点统计</h1>
            <p className="mt-2 text-sm text-gray-500">独立统计页：普通用户进入页面与功能使用次数，按用户聚合。</p>
          </div>
          <Link to="/admin" className="rounded-full bg-gray-900 px-3 py-1.5 text-xs font-semibold text-white hover:bg-gray-700">
            返回管理员后台
          </Link>
        </div>
      </header>
      <AdminUsageAnalyticsSection />
    </div>
  );
}
