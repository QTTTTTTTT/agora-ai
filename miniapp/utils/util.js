/**
 * 工具函数集合
 */

/**
 * 格式化时间 YYYY-MM-DD HH:mm:ss
 * @param {Date|number|string} date
 * @returns {string}
 */
function formatTime(date) {
  if (!(date instanceof Date)) {
    date = new Date(date);
  }
  var year = date.getFullYear();
  var month = date.getMonth() + 1;
  var day = date.getDate();
  var hour = date.getHours();
  var minute = date.getMinutes();
  var second = date.getSeconds();
  return (
    [year, month, day].map(formatNumber).join('-') +
    ' ' +
    [hour, minute, second].map(formatNumber).join(':')
  );
}

/**
 * 格式化日期 YYYY-MM-DD
 * @param {Date|number|string} date
 * @returns {string}
 */
function formatDate(date) {
  if (!(date instanceof Date)) {
    date = new Date(date);
  }
  var year = date.getFullYear();
  var month = date.getMonth() + 1;
  var day = date.getDate();
  return [year, month, day].map(formatNumber).join('-');
}

/**
 * 补零
 * @param {number} n
 * @returns {string}
 */
function formatNumber(n) {
  n = n.toString();
  return n.length < 2 ? '0' + n : n;
}

/**
 * 格式化金额（千分位，保留2位小数）
 * @param {number} num
 * @returns {string}
 */
function formatMoney(num) {
  if (num === null || num === undefined || isNaN(num)) return '--';
  var n = parseFloat(num);
  var isNegative = n < 0;
  n = Math.abs(n);
  var fixed = n.toFixed(2);
  var parts = fixed.split('.');
  var intPart = parts[0];
  var decPart = parts[1];
  // 添加千分位
  var result = '';
  var count = 0;
  for (var i = intPart.length - 1; i >= 0; i--) {
    result = intPart[i] + result;
    count++;
    if (count % 3 === 0 && i > 0) {
      result = ',' + result;
    }
  }
  return (isNegative ? '-' : '') + result + '.' + decPart;
}

/**
 * 格式化百分比 +/-xx.xx%
 * @param {number} num - 小数形式，如 0.0523 表示 5.23%
 * @returns {string}
 */
function formatPercent(num) {
  if (num === null || num === undefined || isNaN(num)) return '--';
  var n = parseFloat(num);
  var pct = (n * 100).toFixed(2);
  var prefix = n > 0 ? '+' : '';
  return prefix + pct + '%';
}

/**
 * 格式化净值（4位小数）
 * @param {number} num
 * @returns {string}
 */
function formatNav(num) {
  if (num === null || num === undefined || isNaN(num)) return '--';
  return parseFloat(num).toFixed(4);
}

/**
 * 截断字符串
 * @param {string} str
 * @param {number} len - 最大长度
 * @returns {string}
 */
function truncate(str, len) {
  if (!str) return '';
  len = len || 20;
  if (str.length <= len) return str;
  return str.substring(0, len) + '...';
}

/**
 * 防抖函数
 * @param {Function} fn
 * @param {number} delay - 延迟毫秒数，默认 300
 * @returns {Function}
 */
function debounce(fn, delay) {
  delay = delay || 300;
  var timer = null;
  return function () {
    var context = this;
    var args = arguments;
    if (timer) clearTimeout(timer);
    timer = setTimeout(function () {
      fn.apply(context, args);
    }, delay);
  };
}

/**
 * 状态 → 颜色映射
 * @param {string} status
 * @returns {string}
 */
function getStatusColor(status) {
  var map = {
    active: '#52c41a',
    running: '#52c41a',
    online: '#52c41a',
    success: '#52c41a',
    completed: '#52c41a',
    approved: '#52c41a',
    idle: '#1890ff',
    pending: '#faad14',
    waiting: '#faad14',
    warning: '#faad14',
    review: '#faad14',
    inactive: '#d9d9d9',
    offline: '#d9d9d9',
    disabled: '#d9d9d9',
    error: '#ff4d4f',
    failed: '#ff4d4f',
    rejected: '#ff4d4f',
    stopped: '#ff4d4f',
  };
  return map[status] || '#999999';
}

/**
 * 角色 → emoji 映射
 * @param {string} role
 * @returns {string}
 */
function getRoleIcon(role) {
  var map = {
    pm: '\uD83D\uDC68\u200D\uD83D\uDCBC',           // 👨‍💼
    fund_manager: '\uD83D\uDC68\u200D\uD83D\uDCBC',  // 👨‍💼
    researcher: '\uD83D\uDD2C',                        // 🔬
    analyst: '\uD83D\uDD2C',                           // 🔬
    risk: '\uD83D\uDEE1\uFE0F',                       // 🛡️
    risk_manager: '\uD83D\uDEE1\uFE0F',               // 🛡️
    trader: '\uD83D\uDCCA',                            // 📊
    executor: '\uD83D\uDCCA',                          // 📊
    compliance: '\uD83D\uDCCB',                        // 📋
    strategist: '\uD83C\uDFAF',                        // 🎯
  };
  return map[role] || '\uD83E\uDD16'; // 🤖
}

/**
 * 角色 → 中文名映射
 * @param {string} role
 * @returns {string}
 */
function getRoleName(role) {
  var map = {
    pm: '基金经理',
    fund_manager: '基金经理',
    researcher: '研究员',
    analyst: '分析师',
    risk: '风控经理',
    risk_manager: '风控经理',
    trader: '交易员',
    executor: '执行员',
    compliance: '合规专员',
    strategist: '策略师',
  };
  return map[role] || role || '未知角色';
}

/**
 * 投资风格 → 中文名映射
 * @param {string} style
 * @returns {string}
 */
function getStyleName(style) {
  var map = {
    conservative: '保守型',
    moderate: '稳健型',
    aggressive: '激进型',
    balanced: '均衡型',
    growth: '成长型',
    value: '价值型',
    momentum: '动量型',
    contrarian: '逆向型',
    quantitative: '量化型',
    index: '指数型',
  };
  return map[style] || style || '未知风格';
}

module.exports = {
  formatTime: formatTime,
  formatDate: formatDate,
  formatMoney: formatMoney,
  formatPercent: formatPercent,
  formatNav: formatNav,
  truncate: truncate,
  debounce: debounce,
  getStatusColor: getStatusColor,
  getRoleIcon: getRoleIcon,
  getRoleName: getRoleName,
  getStyleName: getStyleName,
};
