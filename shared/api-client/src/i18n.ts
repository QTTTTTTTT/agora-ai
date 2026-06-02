/**
 * Shared i18n strings — web + Android 共享。
 *
 * Web 端用自己的 PreferencesProvider 路径，但所有"业务文案"（auth /
 * tabs / decisions / memory / team / more）都通过这里统一定义，避免
 * "web 改了文案 Android 没跟上"的产线漂移。
 *
 * 字典结构是嵌套 record，调用方按 dot path 解析 — 与 i18next 兼容。
 */

export type LocaleId = 'zh-CN' | 'en-US';

export interface Messages {
  auth: {
    loginTitle: string;
    email: string;
    password: string;
    submit: string;
    submitting: string;
    forgot: string;
    errorInvalid: string;
    errorGeneric: string;
    biometricsPrompt: string;
    biometricsRequired: string;
    biometricsBlockedHint: string;
    sessionErrorTitle: string;
    sessionErrorHint: string;
    sessionErrorRetry: string;
    forgotTitle: string;
    forgotHint: string;
    forgotSubmit: string;
    forgotSent: string;
    backToLogin: string;
    resetTitle: string;
    resetHint: string;
    resetNewPassword: string;
    resetConfirmPassword: string;
    resetSubmit: string;
    resetSubmitting: string;
    resetSuccess: string;
    resetTokenInvalid: string;
    resetPasswordMismatch: string;
    twoFATitle: string;
    twoFASubtitle: string;
    twoFAModeCode: string;
    twoFAModeRecovery: string;
    twoFACodePlaceholder: string;
    twoFARecoveryPlaceholder: string;
    twoFASubmit: string;
    twoFACancel: string;
    twoFAInvalidCode: string;
  };
  tabs: {
    home: string;
    decisions: string;
    memory: string;
    team: string;
    more: string;
    orders: string;
  };
  home: {
    title: string;
    empty: string;
    loading: string;
    error: string;
    retry: string;
    navLabel: string;
    assetsLabel: string;
  };
  decisions: {
    title: string;
    empty: string;
    loadFailed: string;
    retry: string;
    actionsLabel: string;
    approve: string;
    reject: string;
    refresh: string;
    rejecting: string;
    approving: string;
    refreshing: string;
    rejectReasonPrompt: string;
    rejectReasonRequired: string;
    confirm: string;
    cancel: string;
    successApproved: string;
    successRejected: string;
    successRefreshed: string;
    actionFailed: string;
    statusDraft: string;
    statusRiskReview: string;
    statusPendingUser: string;
    statusApproved: string;
    statusRejected: string;
    statusExecuting: string;
    statusCompleted: string;
    statusFailed: string;
    statusMixed: string;
    recentEvents: string;
  };
  memory: {
    title: string;
    tabs: { agent: string; reflection: string };
    empty: string;
    error: string;
    retry: string;
  };
  team: {
    title: string;
    empty: string;
    error: string;
    retry: string;
  };
  orders: {
    title: string;
    empty: string;
    loadFailed: string;
    retry: string;
    actionsLabel: string;
    cancel: string;
    replace: string;
    cancelling: string;
    replacing: string;
    cancelConfirmTitle: string;
    cancelConfirmBody: string;
    cancelOk: string;
    cancelOkConfirm: string;
    cancelDismiss: string;
    cancelSuccess: string;
    replaceTitle: string;
    replaceQuantity: string;
    replaceLimit: string;
    replaceStop: string;
    replaceTrailAmount: string;
    replaceTrailPercent: string;
    replaceDisplayQty: string;
    replaceNote: string;
    replaceLeaveBlankHint: string;
    replaceSubmit: string;
    replaceCancel: string;
    replaceSuccess: string;
    actionFailed: string;
    stepUpCancelReason: string;
    stepUpReplaceReason: string;
    // P0-9: live trading hard gate — banner + per-pillar labels.
    liveBannerTitle: string;
    liveBannerSubtitle: string;
    liveBannerEnforced: string;
    liveBannerBypass: string;
    livePillarKYC: string;
    livePillarBrokerLink: string;
    livePillarTwoFA: string;
    livePillarStepUp: string;
    livePillarOK: string;
    livePillarMissing: string;
    liveBlockedKYC: string;
    liveBlockedBrokerLink: string;
    liveBlockedTwoFA: string;
    liveBlockedStepUp: string;
    columns: {
      symbol: string;
      side: string;
      qty: string;
      price: string;
      status: string;
    };
  };
  more: {
    title: string;
    language: string;
    logout: string;
    version: string;
    darkMode: string;
    appearanceSystem: string;
    appearanceLight: string;
    appearanceDark: string;
    accountSecurity: string;
    accountInfoLabel: string;
    accountInfoMissing: string;
    accountEmailVerifiedOn: string;
    accountEmailVerifiedOff: string;
    changePassword: string;
    biometric: string;
    biometricOn: string;
    biometricOff: string;
    biometricHint: string;
    biometricUnavailable: string;
    notifications: string;
    notificationsOn: string;
    notificationsOff: string;
    notificationsHint: string;
    notificationsRegistering: string;
    notificationsRegistrationFailed: string;
    sectionAccount: string;
    sectionAppearance: string;
    sectionLanguage: string;
    sectionDanger: string;
    recentEvents: string;
    twoFATitle: string;
    twoFAHintLoading: string;
    twoFAHintEnabled: string;
    twoFAHintDisabled: string;
    twoFAStatusOn: string;
    twoFAStatusOff: string;
    stepUpOrders: string;
    stepUpOrdersHint: string;
  };
  brokerLinks: {
    title: string;
    subtitle: string;
    formTitle: string;
    formBroker: string;
    formAccountId: string;
    formAccountIdPlaceholder: string;
    formSubmit: string;
    formSubmitting: string;
    formNote: string;
    refresh: string;
    empty: string;
    loading: string;
    revoke: string;
    revoking: string;
    confirmRevoke: string;
    statusPending: string;
    statusActive: string;
    statusSuspended: string;
    statusRevoked: string;
    errorPrefix: string;
  };
  funding: {
    title: string;
    subtitle: string;
    formTitle: string;
    formDirection: string;
    formDirectionDeposit: string;
    formDirectionWithdrawal: string;
    formAmount: string;
    formAmountPlaceholder: string;
    formCurrency: string;
    formMethod: string;
    formExternalReference: string;
    formExternalReferencePlaceholder: string;
    formNotes: string;
    formNotesPlaceholder: string;
    formSubmit: string;
    formSubmitting: string;
    formNote: string;
    methodWire: string;
    methodACH: string;
    methodSEPA: string;
    methodCheck: string;
    methodInternal: string;
    methodManual: string;
    refresh: string;
    empty: string;
    loading: string;
    cancel: string;
    cancelling: string;
    confirmCancel: string;
    statusPending: string;
    statusApproved: string;
    statusRejected: string;
    statusCancelled: string;
    statusPosted: string;
    rejectionReasonLabel: string;
    awaitingApproval: string;
    errorPrefix: string;
    insufficientCash: string;
  };
  fx: {
    panelTitle: string;
    panelSubtitle: string;
    listEmpty: string;
    listLoading: string;
    listError: string;
    refresh: string;
    pairLabel: string;
    rateLabel: string;
    rateAtLabel: string;
    sourceLabel: string;
    formTitle: string;
    formBase: string;
    formQuote: string;
    formRate: string;
    formRatePlaceholder: string;
    formSource: string;
    formSourceManual: string;
    formSourceOverride: string;
    formNote: string;
    formNotePlaceholder: string;
    formSubmit: string;
    formSubmitting: string;
    formSuccess: string;
    sourceManual: string;
    sourceOverride: string;
    sourceYahoo: string;
    sourceEod: string;
    fundBaseCurrencyLabel: string;
    fundBaseCurrencyHint: string;
    fundBaseCurrencySaving: string;
    fundBaseCurrencySaved: string;
    fxStaleBanner: string;
  };
  recon: {
    panelTitle: string;
    panelSubtitle: string;
    listEmpty: string;
    listLoading: string;
    listError: string;
    refresh: string;
    runDateLabel: string;
    triggerSourceLabel: string;
    statusLabel: string;
    breakCountLabel: string;
    breakCountCriticalLabel: string;
    breakCountWarningLabel: string;
    breakCountInfoLabel: string;
    severityCritical: string;
    severityWarning: string;
    severityInfo: string;
    statusOpen: string;
    statusAcknowledged: string;
    statusResolved: string;
    statusIgnored: string;
    statusPending: string;
    statusCompleted: string;
    statusFailed: string;
    triggerSourceManual: string;
    triggerSourceScheduled: string;
    triggerSourceReplay: string;
    breakTypePositionQuantity: string;
    breakTypePositionAvgCost: string;
    breakTypePositionMissingInternal: string;
    breakTypePositionMissingBroker: string;
    breakTypeCashBalance: string;
    breakTypeCashCurrencyMissingInternal: string;
    breakTypeCashCurrencyMissingBroker: string;
    breakTypeTradeMissingInternal: string;
    breakTypeTradeMissingBroker: string;
    breakTypeTradeQuantity: string;
    breakTypeTradePrice: string;
    breakTypeTradeSide: string;
    triggerRunButton: string;
    triggerRunDialogTitle: string;
    triggerRunFundIdLabel: string;
    triggerRunFundIdPlaceholder: string;
    triggerRunUseMockLabel: string;
    triggerRunDriftQtyLabel: string;
    triggerRunDriftCashLabel: string;
    triggerRunDriftPriceLabel: string;
    triggerRunSubmit: string;
    triggerRunSubmitting: string;
    triggerRunSuccess: string;
    triggerRunError: string;
    breakActionAcknowledge: string;
    breakActionResolve: string;
    breakActionIgnore: string;
    breakActionReopen: string;
    breakResolveDialogTitle: string;
    breakResolveNoteLabel: string;
    breakResolveSubmit: string;
    breakResolveSubmitting: string;
    breakDrillDownTitle: string;
    breakDetailInternalValue: string;
    breakDetailBrokerValue: string;
    breakDetailDiffValue: string;
    breakDetailDiffPercent: string;
    breakDetailDescription: string;
    breakDetailMetadata: string;
    drillDownNoBreaks: string;
  };
  surveillance: {
    panelTitle: string;
    panelSubtitle: string;
    listEmpty: string;
    listLoading: string;
    listError: string;
    refresh: string;
    detectedAtLabel: string;
    ruleCodeLabel: string;
    severityLabel: string;
    statusLabel: string;
    symbolLabel: string;
    summaryLabel: string;
    triggerScanButton: string;
    triggerScanDialogTitle: string;
    triggerScanFundIdLabel: string;
    triggerScanFundIdPlaceholder: string;
    triggerScanAsOfLabel: string;
    triggerScanSessionCloseLabel: string;
    triggerScanSubmit: string;
    triggerScanSubmitting: string;
    triggerScanSuccess: string;
    triggerScanError: string;
    severityCritical: string;
    severityWarning: string;
    severityInfo: string;
    statusOpen: string;
    statusReviewing: string;
    statusCleared: string;
    statusEscalated: string;
    triggerSourceManual: string;
    triggerSourceScheduled: string;
    ruleWashTrade: string;
    ruleMarkingClose: string;
    ruleSelfTradePair: string;
    ruleRapidFireReversal: string;
    ruleLayeringSuspect: string;
    eventActionAcknowledge: string;
    eventActionClear: string;
    eventActionEscalate: string;
    eventActionReopen: string;
    eventReviewDialogTitle: string;
    eventReviewNoteLabel: string;
    eventReviewSubmit: string;
    eventReviewSubmitting: string;
    eventDetailMetadata: string;
    eventDetailTradeIDs: string;
    eventDetailWindow: string;
    runsSubpanelTitle: string;
    runsTradeCountLabel: string;
    runsEventCountLabel: string;
    runsDurationLabel: string;
  };
  drawdown: {
    panelTitle: string;
    panelSubtitle: string;
    refresh: string;
    listEmpty: string;
    listLoading: string;
    listError: string;
    fundIdLabel: string;
    fundIdPlaceholder: string;
    loadFundButton: string;
    statusTitle: string;
    peakNavLabel: string;
    currentNavLabel: string;
    currentDDLabel: string;
    hasPolicyTrue: string;
    hasPolicyFalse: string;
    breachedTierLabel: string;
    triggerCheckButton: string;
    triggerCheckRunning: string;
    triggerCheckNoBreach: string;
    triggerCheckBreached: string;
    triggerCheckError: string;
    tiersTitle: string;
    tierLabel: string;
    ddPctLabel: string;
    actionLabel: string;
    trimRatioLabel: string;
    cooldownLabel: string;
    autoExecuteLabel: string;
    noteLabel: string;
    addTierButton: string;
    saveTierButton: string;
    saveTierSubmitting: string;
    deleteTierButton: string;
    deleteConfirm: string;
    actionTrimProportional: string;
    actionFlatten: string;
    actionDefensiveOnly: string;
    eventsTitle: string;
    detectedAtLabel: string;
    statusLabel: string;
    statusProposed: string;
    statusApproved: string;
    statusExecuted: string;
    statusDismissed: string;
    statusSuperseded: string;
    trimPlanTitle: string;
    trimPlanEmpty: string;
    eventActionApprove: string;
    eventActionDismiss: string;
    eventActionReopen: string;
    reviewDialogTitle: string;
    reviewNoteLabel: string;
    reviewSubmit: string;
    reviewSubmitting: string;
    reviewError: string;
  };
  marketStatus: {
    panelTitle: string;
    panelSubtitle: string;
    refresh: string;
    instrumentsTitle: string;
    instrumentsEmpty: string;
    fieldKey: string;
    fieldSymbol: string;
    fieldMarket: string;
    fieldStatus: string;
    fieldHaltReason: string;
    fieldHaltUntil: string;
    fieldLower: string;
    fieldUpper: string;
    fieldLastQuoteAt: string;
    fieldStalenessBudget: string;
    statusTrading: string;
    statusHalted: string;
    statusSuspended: string;
    haltButton: string;
    haltSubmitting: string;
    haltDialogTitle: string;
    haltReasonLabel: string;
    haltUntilLabel: string;
    unhaltButton: string;
    setLimitsButton: string;
    setLimitsDialogTitle: string;
    upsertDialogTitle: string;
    saveButton: string;
    saveSubmitting: string;
    cancelButton: string;
    eventsTitle: string;
    eventDecision: string;
    eventRule: string;
    eventSummary: string;
    eventDetected: string;
    decisionAllow: string;
    decisionWarn: string;
    decisionReject: string;
    ruleHalted: string;
    ruleSuspended: string;
    rulePriceLimit: string;
    ruleStaleQuote: string;
    ruleMarketClosed: string;
    ruleHalfDayClosed: string;
    calendarTitle: string;
    calendarMarketLabel: string;
    calendarFromLabel: string;
    calendarToLabel: string;
    calendarLoadButton: string;
    calendarUpsertTitle: string;
    calendarIsOpen: string;
    calendarHalfDay: string;
    calendarOpenLocal: string;
    calendarCloseLocal: string;
    calendarTZ: string;
    calendarNote: string;
    error: string;
  };
  marketImpact: {
    panelTitle: string;
    panelSubtitle: string;
    refresh: string;
    instrumentsTitle: string;
    instrumentsEmpty: string;
    fieldKey: string;
    fieldSymbol: string;
    fieldMarket: string;
    fieldAssetClass: string;
    fieldADV: string;
    fieldADVNotional: string;
    fieldVolatility: string;
    fieldImpactCoef: string;
    fieldImpactExp: string;
    fieldMinBps: string;
    fieldMaxBps: string;
    fieldLastCalibrated: string;
    fieldSource: string;
    upsertButton: string;
    upsertDialogTitle: string;
    deleteButton: string;
    deleteConfirm: string;
    saveButton: string;
    saveSubmitting: string;
    cancelButton: string;
    sourceManual: string;
    sourceHistorical: string;
    sourceBrokerReported: string;
    previewTitle: string;
    previewSubtitle: string;
    previewSide: string;
    previewSideBuy: string;
    previewSideSell: string;
    previewQuantity: string;
    previewReferencePrice: string;
    previewSubmit: string;
    previewSubmitting: string;
    previewResult: string;
    previewBps: string;
    previewImpliedFill: string;
    previewImpactCost: string;
    previewUsedDefaults: string;
    previewUsedADVFallback: string;
    cacheTitle: string;
    cacheSize: string;
    cacheLastRefresh: string;
    cacheRefreshButton: string;
    cacheRefreshing: string;
    error: string;
  };
  lockup: {
    panelTitle: string;
    panelSubtitle: string;
    refresh: string;
    listTitle: string;
    listEmpty: string;
    fieldFund: string;
    fieldInstrument: string;
    fieldSymbol: string;
    fieldQty: string;
    fieldUntil: string;
    fieldReason: string;
    fieldNote: string;
    fieldStatus: string;
    fieldSourceLot: string;
    fieldReleasedAt: string;
    fieldReleasedReason: string;
    statusActive: string;
    statusExpired: string;
    statusReleased: string;
    reasonIPO: string;
    reasonPrivatePlacement: string;
    reasonRSU: string;
    reasonRestricted: string;
    reasonEmployeeGrant: string;
    reasonBlockSale: string;
    reasonOther: string;
    filterAll: string;
    createButton: string;
    createDialogTitle: string;
    editButton: string;
    editDialogTitle: string;
    deleteButton: string;
    deleteConfirm: string;
    releaseButton: string;
    releaseDialogTitle: string;
    releaseReasonLabel: string;
    saveButton: string;
    saveSubmitting: string;
    cancelButton: string;
    error: string;
  };
  borrow: {
    panelTitle: string;
    panelSubtitle: string;
    refresh: string;
    listTitle: string;
    listEmpty: string;
    fieldKey: string;
    fieldSymbol: string;
    fieldMarket: string;
    fieldRate: string;
    fieldLocateFee: string;
    fieldAvailability: string;
    fieldAvailable: string;
    fieldMinLocate: string;
    fieldMaxLocate: string;
    fieldSource: string;
    fieldNote: string;
    availEasy: string;
    availHard: string;
    availRestricted: string;
    availUnavailable: string;
    sourceManual: string;
    sourceBrokerQuote: string;
    sourceAgentLender: string;
    sourceHistorical: string;
    sourcePublicFeed: string;
    upsertButton: string;
    upsertSubmitting: string;
    deleteButton: string;
    cacheTitle: string;
    cacheSize: string;
    cacheLastRefresh: string;
    cacheRefreshButton: string;
    cacheRefreshing: string;
    previewTitle: string;
    previewSubtitle: string;
    previewFundLabel: string;
    previewKeyLabel: string;
    previewQtyLabel: string;
    previewPriceLabel: string;
    previewSubmit: string;
    previewSubmitting: string;
    previewResultDecision: string;
    previewResultRate: string;
    previewResultLocateFee: string;
    previewResultNotional: string;
    auditTitle: string;
    auditFundFilter: string;
    auditDecisionFilter: string;
    auditEmpty: string;
    ledgerTitle: string;
    ledgerEmpty: string;
    error: string;
  };
  wsfeed: {
    panelTitle: string;
    panelSubtitle: string;
    disabled: string;
    refresh: string;
    reconcile: string;
    reconcileSubmitting: string;
    statusEnabled: string;
    statusHealthyProviders: string;
    statusSubscriptions: string;
    statusCacheSymbols: string;
    statusTotalTicks: string;
    statusDroppedEvents: string;
    connectionsTitle: string;
    connectionsEmpty: string;
    colProvider: string;
    colState: string;
    colTickCount: string;
    colReconnects: string;
    colLastTick: string;
    colConnectedAt: string;
    colLastError: string;
    stateConnected: string;
    stateConnecting: string;
    stateReconnecting: string;
    stateBackoff: string;
    stateDisconnected: string;
    stateClosed: string;
    stateUnknown: string;
    subscriptionsTitle: string;
    subscriptionsEmpty: string;
    colSymbol: string;
    colMarket: string;
    colConsumers: string;
    cacheTitle: string;
    cacheStats: string;
    cacheEmpty: string;
    colLast: string;
    colBid: string;
    colAsk: string;
    colAsOf: string;
    colStale: string;
    subscribeTitle: string;
    subscribeSymbolPlaceholder: string;
    subscribeMarketPlaceholder: string;
    subscribeSubmit: string;
    subscribeSubmitting: string;
    unsubscribeButton: string;
    evictCacheTitle: string;
    evictCacheButton: string;
    evictCacheAllButton: string;
    error: string;
  };
  factorExposure: {
    panelTitle: string;
    panelSubtitle: string;
    refresh: string;
    loading: string;
    empty: string;
    error: string;
    navLabel: string;
    holdingsLabel: string;
    coverageLabel: string;
    loadingsAsOfLabel: string;
    loadingsAsOfStale: string;
    factorSize: string;
    factorValue: string;
    factorMomentum: string;
    factorQuality: string;
    factorLowVol: string;
    factorMarketBeta: string;
    netExposureLabel: string;
    grossExposureLabel: string;
    holdingCountLabel: string;
    trendTitle: string;
    trendEmpty: string;
    adminPanelTitle: string;
    adminPanelSubtitle: string;
    adminListTitle: string;
    adminListEmpty: string;
    adminInstrumentKey: string;
    adminFactorLabel: string;
    adminAsOfLabel: string;
    adminLoadingLabel: string;
    adminSourceLabel: string;
    adminNoteLabel: string;
    adminUpdatedAtLabel: string;
    adminFactorAll: string;
    adminUpsertTitle: string;
    adminUpsertSubmit: string;
    adminUpsertSubmitting: string;
    adminDeleteButton: string;
    adminDeleteConfirm: string;
    sourceManual: string;
    sourceEastMoney: string;
    sourceMSCI: string;
    sourceComputed: string;
    sourceOverride: string;
  };
  varRisk: {
    panelTitle: string;
    panelSubtitle: string;
    refresh: string;
    loading: string;
    empty: string;
    error: string;
    insufficientHistory: string;
    sampleSizeLabel: string;
    lookbackLabel: string;
    horizonLabel: string;
    horizon1d: string;
    horizon5d: string;
    horizon10d: string;
    meanLabel: string;
    stdevLabel: string;
    sampleWindowLabel: string;
    methodLabel: string;
    confidenceLabel: string;
    varLabel: string;
    cvarLabel: string;
    methodHistorical: string;
    methodParametric: string;
    methodMonteCarlo: string;
    methodHistoricalSubtitle: string;
    methodParametricSubtitle: string;
    methodMonteCarloSubtitle: string;
    confidence90Label: string;
    confidence95Label: string;
    confidence99Label: string;
    varInterpretation: string;
    cvarInterpretation: string;
  };
  stressTest: {
    panelTitle: string;
    panelSubtitle: string;
    runButton: string;
    running: string;
    refresh: string;
    empty: string;
    error: string;
    scenarioLabel: string;
    scenarioPlaceholder: string;
    categoryLabel: string;
    descriptionLabel: string;
    shockCountLabel: string;
    navBeforeLabel: string;
    navAfterLabel: string;
    pnlTotalLabel: string;
    pnlPctLabel: string;
    holdingsLabel: string;
    shockedLabel: string;
    impactsTitle: string;
    impactsEmpty: string;
    impactSymbol: string;
    impactBefore: string;
    impactAfter: string;
    impactPnL: string;
    impactReturn: string;
    impactShock: string;
    categoryHistorical: string;
    categoryHypothetical: string;
    categoryRegulatory: string;
    adminPanelTitle: string;
    adminPanelSubtitle: string;
    adminListTitle: string;
    adminListEmpty: string;
    adminScenarioName: string;
    adminScenarioCategory: string;
    adminScenarioDescription: string;
    adminScenarioShocks: string;
    adminScenarioCreatedBy: string;
    adminScenarioUpdatedAt: string;
    adminUpsertTitle: string;
    adminUpsertSubmit: string;
    adminUpsertSubmitting: string;
    adminDeleteButton: string;
    adminDeleteConfirm: string;
    targetInstrument: string;
    targetMarket: string;
    targetAssetClass: string;
    targetFactor: string;
    targetWildcard: string;
  };
  brinsonAttribution: {
    panelTitle: string;
    panelSubtitle: string;
    runButton: string;
    running: string;
    benchmarkLabel: string;
    benchmarkPlaceholder: string;
    dimensionLabel: string;
    dimensionAssetClass: string;
    dimensionMarket: string;
    dimensionSector: string;
    benchmarkEmpty: string;
    portfolioReturn: string;
    benchmarkReturn: string;
    activeReturn: string;
    allocationEffect: string;
    selectionEffect: string;
    interactionEffect: string;
    totalEffect: string;
    decompositionTitle: string;
    bucketsTitle: string;
    bucketsEmpty: string;
    colBucket: string;
    colPortfolioWeight: string;
    colBenchmarkWeight: string;
    colPortfolioReturn: string;
    colBenchmarkReturn: string;
    colAllocation: string;
    colSelection: string;
    colInteraction: string;
    colTotal: string;
    persistLabel: string;
    error: string;
    noPortfolioHoldings: string;
    compositionNotFound: string;
    sectorUnsupported: string;
    asofLabel: string;
    adminPanelTitle: string;
    adminPanelSubtitle: string;
    adminListTitle: string;
    adminListEmpty: string;
    adminUpsertTitle: string;
    adminUpsertSubmit: string;
    adminUpsertSubmitting: string;
    adminDeleteButton: string;
    adminDeleteConfirm: string;
    adminBucketKey: string;
    adminBucketWeight: string;
    adminBucketReturn: string;
    adminAddBucket: string;
    adminRemoveBucket: string;
    adminBenchmarkId: string;
    adminAsof: string;
    adminNote: string;
  };
  analystPanel: {
    title: string;
    subtitle: string;
    symbolLabel: string;
    symbolPlaceholder: string;
    runButton: string;
    running: string;
    persistLabel: string;
    aggregateTitle: string;
    aggregateDirection: string;
    aggregateConfidence: string;
    categoriesVoted: string;
    voteSummary: string;
    perCategoryTitle: string;
    asof: string;
    generatedAt: string;
    directionBullish: string;
    directionBearish: string;
    directionNeutral: string;
    categoryFundamentals: string;
    categorySentiment: string;
    categoryNews: string;
    categoryTechnical: string;
    thesisLabel: string;
    keyFindingsLabel: string;
    risksLabel: string;
    dataPointsLabel: string;
    sourcesLabel: string;
    noPanelYet: string;
    error: string;
    historyTitle: string;
    historyEmpty: string;
    historyLoading: string;
    confidenceLabel: string;
    llmModelFallback: string;
    llmModelLLM: string;
  };
  bullBearDebate: {
    title: string;
    subtitle: string;
    symbolLabel: string;
    symbolPlaceholder: string;
    roundsLabel: string;
    runButton: string;
    running: string;
    notesLabel: string;
    verdictTitle: string;
    verdictDirection: string;
    verdictConfidence: string;
    verdictContested: string;
    verdictNotContested: string;
    bullConfidence: string;
    bearConfidence: string;
    winnerBull: string;
    winnerBear: string;
    winnerTie: string;
    argumentsTitle: string;
    roundLabel: string;
    stanceBull: string;
    stanceBear: string;
    thesisLabel: string;
    supportPointsLabel: string;
    rebuttalsLabel: string;
    citedReportsLabel: string;
    noDebateYet: string;
    error: string;
    historyTitle: string;
    historyEmpty: string;
    historyLoading: string;
    confidenceLabel: string;
    llmModelFallback: string;
    llmModelLLM: string;
  };
  corpActions: {
    title: string;
    subtitle: string;
    expand: string;
    collapse: string;
    loading: string;
    error: string;
    retry: string;
    empty: string;
    typeSplit: string;
    typeCashDividend: string;
    typeStockDividend: string;
    typeCombined: string;
    sharesLabel: string;
    costLabel: string;
    cashLabel: string;
    exDateLabel: string;
  };
  benchmark: {
    title: string;
    subtitle: string;
    fund: string;
    days7: string;
    days30: string;
    days90: string;
    days180: string;
    days365: string;
    expand: string;
    collapse: string;
    loading: string;
    empty: string;
    error: string;
    retry: string;
    seriesPicker: string;
    addSeries: string;
    partialFailureToast: string;
    legendStart: string;
    holdingOverlapDominantTitle: string;
    holdingOverlapDominantBody: string;
    holdingOverlapPartialTitle: string;
    holdingOverlapPartialBody: string;
    holdingOverlapSwitchToAlpha: string;
  };
  holdingsSeries: {
    title: string;
    subtitle: string;
    expand: string;
    collapse: string;
    loading: string;
    error: string;
    retry: string;
    empty: string;
    vsEntry: string;
    vsStart: string;
    partialFailureToast: string;
    days30: string;
    days90: string;
    days180: string;
  };
  abShadow: {
    sectionTitle: string;
    sectionSubtitle: string;
    expand: string;
    collapse: string;
    loading: string;
    error: string;
    retry: string;
    empty: string;
    notAnalyzedYet: string;
    columnA: string;
    columnB: string;
    eventCount: string;
    latestDate: string;
    lessons: string;
    adjustments: string;
    summaries: string;
    timeline: string;
    memories: string;
    proposedDiff: string;
    diffAdded: string;
    diffChanged: string;
    diffRemoved: string;
    noDiff: string;
    deterministicShadowBanner: string;
  };
  abAttribution: {
    sectionTitle: string;
    sectionSubtitle: string;
    expand: string;
    collapse: string;
    loading: string;
    error: string;
    retry: string;
    empty: string;
    columnSymbol: string;
    columnTradesA: string;
    columnTradesB: string;
    columnPnLA: string;
    columnPnLB: string;
    columnTurnoverA: string;
    columnTurnoverB: string;
    columnGap: string;
    columnGapPct: string;
    columnWinner: string;
    winnerA: string;
    winnerB: string;
    winnerTie: string;
    totalsTitle: string;
    avgPnL: string;
    winRate: string;
  };
}

export const messages: Record<LocaleId, Messages> = {
  'zh-CN': {
    auth: {
      loginTitle: '登录',
      email: '邮箱',
      password: '密码',
      submit: '登录',
      submitting: '登录中…',
      forgot: '忘记密码？',
      errorInvalid: '邮箱或密码错误',
      errorGeneric: '登录失败，请稍后再试',
      biometricsPrompt: '使用生物识别解锁',
      biometricsRequired: '生物识别验证失败',
      biometricsBlockedHint: '生物识别失败或被取消，请改用密码登录。',
      sessionErrorTitle: '当前无法连接服务',
      sessionErrorHint: '网络异常或服务端暂时不可用，重试或重新登录均可。',
      sessionErrorRetry: '重试连接',
      forgotTitle: '重置密码',
      forgotHint: '我们会向该邮箱发送一封带链接的邮件。',
      forgotSubmit: '发送邮件',
      forgotSent: '已发送，请查收邮箱。',
      backToLogin: '返回登录',
      resetTitle: '设置新密码',
      resetHint: '请设置不少于 8 位的密码。重置完成后请重新登录。',
      resetNewPassword: '新密码',
      resetConfirmPassword: '确认新密码',
      resetSubmit: '更新密码',
      resetSubmitting: '更新中…',
      resetSuccess: '密码已更新，请重新登录。',
      resetTokenInvalid: '链接无效或已过期，请回到忘记密码页面重新发起。',
      resetPasswordMismatch: '两次输入的密码不一致。',
      twoFATitle: '二次验证',
      twoFASubtitle: '请输入身份验证器中显示的 6 位验证码。',
      twoFAModeCode: '验证器代码',
      twoFAModeRecovery: '恢复码',
      twoFACodePlaceholder: '6 位验证码',
      twoFARecoveryPlaceholder: '恢复码',
      twoFASubmit: '验证并登录',
      twoFACancel: '更换账号',
      twoFAInvalidCode: '验证码无效，请重试。',
    },
    tabs: { home: '首页', decisions: '决策', memory: '记忆', team: '团队', more: '更多', orders: '订单' },
    home: {
      title: '我的基金',
      empty: '暂无基金，先在 web 端创建。',
      loading: '加载中…',
      error: '加载失败',
      retry: '重试',
      navLabel: '净值',
      assetsLabel: '总资产',
    },
    decisions: {
      title: '最新决策',
      empty: '今天还没有计划生成。',
      loadFailed: '加载决策失败，请重试。',
      retry: '重试',
      actionsLabel: '动作',
      approve: '通过计划',
      reject: '驳回计划',
      refresh: '刷新报价',
      approving: '提交中…',
      rejecting: '驳回中…',
      refreshing: '刷新中…',
      rejectReasonPrompt: '请简述驳回原因（1-200 字）',
      rejectReasonRequired: '驳回需要填写原因',
      confirm: '确认',
      cancel: '取消',
      successApproved: '已通过，等待执行',
      successRejected: '已驳回',
      successRefreshed: '报价已刷新',
      actionFailed: '操作失败，请重试',
      statusDraft: '草稿',
      statusRiskReview: '风控审查中',
      statusPendingUser: '待审批',
      statusApproved: '已通过',
      statusRejected: '已驳回',
      statusExecuting: '执行中',
      statusCompleted: '已完成',
      statusFailed: '失败',
      statusMixed: '部分成交',
      recentEvents: '最近事件',
    },
    memory: {
      title: '记忆与反思',
      tabs: { agent: '每日学习', reflection: '长期反思' },
      empty: '尚未生成记忆。',
      error: '加载失败',
      retry: '重试',
    },
    team: { title: 'Agent 团队', empty: '当前基金未配置 agent。', error: '加载失败', retry: '重试' },
    orders: {
      title: '我的订单',
      empty: '暂无未完成订单。',
      loadFailed: '加载订单失败',
      retry: '重试',
      actionsLabel: '操作',
      cancel: '取消',
      replace: '改单',
      cancelling: '取消中…',
      replacing: '保存中…',
      cancelConfirmTitle: '取消订单',
      cancelConfirmBody: '确定取消该订单？此操作会记入审计日志，且无法撤销。',
      cancelOk: '确定',
      cancelOkConfirm: '取消订单',
      cancelDismiss: '关闭',
      cancelSuccess: '订单已取消。',
      replaceTitle: '修改订单',
      replaceQuantity: '新数量',
      replaceLimit: '新限价',
      replaceStop: '新止损触发价',
      replaceTrailAmount: '新追踪金额',
      replaceTrailPercent: '新追踪百分比 (0-1)',
      replaceDisplayQty: '新冰山显示量',
      replaceNote: '备注（可选）',
      replaceLeaveBlankHint: '留空表示不修改该字段。',
      replaceSubmit: '保存修改',
      replaceCancel: '取消',
      replaceSuccess: '订单已更新。',
      actionFailed: '操作失败',
      stepUpCancelReason: '请通过生物识别确认撤单',
      stepUpReplaceReason: '请通过生物识别确认改单',
      liveBannerTitle: '实盘前置条件',
      liveBannerSubtitle: '该基金为实盘模式，需四项校验全部通过后才能下单/改单/撤单。',
      liveBannerEnforced: '硬门禁已开启',
      liveBannerBypass: '硬门禁未开启（开发模式）',
      livePillarKYC: 'KYC 实名认证',
      livePillarBrokerLink: '券商账户绑定',
      livePillarTwoFA: '2FA / TOTP',
      livePillarStepUp: '生物识别确认',
      livePillarOK: '已通过',
      livePillarMissing: '待完成',
      liveBlockedKYC: '请先完成 KYC 实名认证',
      liveBlockedBrokerLink: '请先绑定券商账户',
      liveBlockedTwoFA: '请先开启 2FA / TOTP',
      liveBlockedStepUp: '请先通过生物识别确认',
      columns: { symbol: '标的', side: '方向', qty: '数量', price: '价格', status: '状态' },
    },
    more: {
      title: '更多',
      language: '语言',
      logout: '退出登录',
      version: '版本',
      darkMode: '暗色模式',
      appearanceSystem: '跟随系统',
      appearanceLight: '浅色',
      appearanceDark: '深色',
      accountSecurity: '账号与安全',
      accountInfoLabel: '当前账号',
      accountInfoMissing: '未获取到账号信息',
      accountEmailVerifiedOn: '邮箱已验证',
      accountEmailVerifiedOff: '邮箱待验证',
      changePassword: '修改密码',
      biometric: '生物识别',
      biometricOn: '已启用',
      biometricOff: '未启用',
      biometricHint: '关闭后下次启动直接进入主界面，不再要求生物识别。',
      biometricUnavailable: '设备未启用指纹/面容识别',
      notifications: '推送通知',
      notificationsOn: '已开启',
      notificationsOff: '已关闭',
      notificationsHint: '决策完成 / 风控异常 / 反思更新等关键事件会发送通知。',
      notificationsRegistering: '正在注册推送…',
      notificationsRegistrationFailed: '推送注册失败，请稍后再试。',
      sectionAccount: '账号',
      sectionAppearance: '界面',
      sectionLanguage: '语言与地区',
      sectionDanger: '会话',
      recentEvents: '最近事件',
      twoFATitle: '二次验证',
      twoFAHintLoading: '正在加载状态…',
      twoFAHintEnabled: '已启用。可在网页端账户安全页修改。',
      twoFAHintDisabled: '未启用。请前往网页端开启。',
      twoFAStatusOn: '已开启',
      twoFAStatusOff: '未开启',
      stepUpOrders: '下单/改单生物识别',
      stepUpOrdersHint: '开启后，每次撤单/改单都会先要求生物识别确认。',
    },
    brokerLinks: {
      title: '券商账户绑定',
      subtitle: '为该基金绑定一个券商账户。新建请求会进入待审批状态，需另一位 super_admin 完成 4-eye 审核后才能用于实盘下单。',
      formTitle: '新建绑定请求',
      formBroker: '券商',
      formAccountId: '券商账号',
      formAccountIdPlaceholder: '如 U1234567',
      formSubmit: '提交申请',
      formSubmitting: '提交中…',
      formNote: '提交后请等待管理员 4-eye 审批；已通过的绑定才会被实盘门禁认可。',
      refresh: '刷新',
      empty: '暂无绑定记录',
      loading: '加载中…',
      revoke: '注销',
      revoking: '注销中…',
      confirmRevoke: '注销该绑定后，实盘下单将被门禁拦截，确定继续？',
      statusPending: '待审批',
      statusActive: '已生效',
      statusSuspended: '已暂停',
      statusRevoked: '已注销',
      errorPrefix: '操作失败：',
    },
    funding: {
      title: '出入金管理',
      subtitle: '提交基金的出入金请求；金额仅在另一位 super_admin 4-eye 审批通过后才会落账并写入 cash_ledger。',
      formTitle: '新建出入金',
      formDirection: '方向',
      formDirectionDeposit: '入金',
      formDirectionWithdrawal: '出金',
      formAmount: '金额',
      formAmountPlaceholder: '如 100000',
      formCurrency: '币种',
      formMethod: '渠道',
      formExternalReference: '外部凭证号',
      formExternalReferencePlaceholder: '如电汇 ref / ACH trace id',
      formNotes: '备注',
      formNotesPlaceholder: '可填写 ticket 编号或场景说明，便于审批人参考',
      formSubmit: '提交申请',
      formSubmitting: '提交中…',
      formNote: '出金会在审批时校验余额是否充足；不足将被拒绝。',
      methodWire: '电汇',
      methodACH: 'ACH',
      methodSEPA: 'SEPA',
      methodCheck: '支票',
      methodInternal: '内部划转',
      methodManual: '人工',
      refresh: '刷新',
      empty: '暂无出入金记录',
      loading: '加载中…',
      cancel: '撤回',
      cancelling: '撤回中…',
      confirmCancel: '撤回后该请求不再被审批；可以重新提交一次新的，确定继续？',
      statusPending: '待审批',
      statusApproved: '已通过',
      statusRejected: '已拒绝',
      statusCancelled: '已撤回',
      statusPosted: '已落账',
      rejectionReasonLabel: '拒绝原因',
      awaitingApproval: '等待管理员 4-eye 审批',
      errorPrefix: '操作失败：',
      insufficientCash: '余额不足，无法批准此次出金',
    },
    fx: {
      panelTitle: 'FX 汇率',
      panelSubtitle: '系统每 6 小时从 Yahoo 抓取 USD 主导对，操作员可在此手动覆盖；NAV 与现金台账按基金 base_currency 折算时使用最近一次 manual / override / yahoo 的值。',
      listEmpty: '暂无 FX 汇率记录',
      listLoading: '加载中…',
      listError: '加载 FX 汇率失败',
      refresh: '刷新',
      pairLabel: '货币对',
      rateLabel: '汇率',
      rateAtLabel: '观察时间',
      sourceLabel: '来源',
      formTitle: '手动覆盖汇率',
      formBase: '基础货币',
      formQuote: '报价货币',
      formRate: '汇率（1 base = ? quote）',
      formRatePlaceholder: '例如 7.18',
      formSource: '来源标记',
      formSourceManual: 'manual（操作员录入）',
      formSourceOverride: 'override（覆盖自动抓取）',
      formNote: '备注（可选）',
      formNotePlaceholder: '说明本次覆盖原因，便于审计回溯',
      formSubmit: '提交',
      formSubmitting: '提交中…',
      formSuccess: '已写入 fx_rates，并记录审计链',
      sourceManual: '人工',
      sourceOverride: '人工覆盖',
      sourceYahoo: 'Yahoo',
      sourceEod: 'EOD',
      fundBaseCurrencyLabel: '基金报告币种',
      fundBaseCurrencyHint: '改为非 USD 后，系统将按 USD-anchored 汇率把所有持仓和现金折算到该币种再展示 NAV。',
      fundBaseCurrencySaving: '保存中…',
      fundBaseCurrencySaved: '已保存',
      fxStaleBanner: '部分币种当前没有最新 FX 汇率，余额按原币种近似计入，仅供参考',
    },
    recon: {
      panelTitle: '日终对账',
      panelSubtitle: '系统每天自动按 mock 券商对账单与持仓 / 现金 / 成交进行 diff，breaks 写入审计链；运维确认后 acknowledge 或 resolve。',
      listEmpty: '暂无对账运行记录',
      listLoading: '加载中…',
      listError: '加载对账记录失败',
      refresh: '刷新',
      runDateLabel: '业务日',
      triggerSourceLabel: '触发方式',
      statusLabel: '状态',
      breakCountLabel: '差异数',
      breakCountCriticalLabel: '严重',
      breakCountWarningLabel: '警告',
      breakCountInfoLabel: '提示',
      severityCritical: '严重',
      severityWarning: '警告',
      severityInfo: '提示',
      statusOpen: '待处理',
      statusAcknowledged: '已确认',
      statusResolved: '已解决',
      statusIgnored: '已忽略',
      statusPending: '运行中',
      statusCompleted: '已完成',
      statusFailed: '失败',
      triggerSourceManual: '手动',
      triggerSourceScheduled: '日终调度',
      triggerSourceReplay: '重放',
      breakTypePositionQuantity: '持仓数量不一致',
      breakTypePositionAvgCost: '持仓成本价不一致',
      breakTypePositionMissingInternal: '内部缺持仓',
      breakTypePositionMissingBroker: '券商缺持仓',
      breakTypeCashBalance: '现金余额不一致',
      breakTypeCashCurrencyMissingInternal: '内部缺该币种现金',
      breakTypeCashCurrencyMissingBroker: '券商缺该币种现金',
      breakTypeTradeMissingInternal: '内部缺成交',
      breakTypeTradeMissingBroker: '券商缺成交',
      breakTypeTradeQuantity: '成交数量不一致',
      breakTypeTradePrice: '成交价格不一致',
      breakTypeTradeSide: '成交方向不一致',
      triggerRunButton: '手动触发对账',
      triggerRunDialogTitle: '手动触发对账运行',
      triggerRunFundIdLabel: '基金 ID',
      triggerRunFundIdPlaceholder: '请输入要对账的基金 UUID',
      triggerRunUseMockLabel: '使用 mock 券商对账单（当前仅支持此模式）',
      triggerRunDriftQtyLabel: '人为持仓数量偏移（演示用）',
      triggerRunDriftCashLabel: '人为现金偏移（演示用）',
      triggerRunDriftPriceLabel: '人为成交价格偏移（演示用）',
      triggerRunSubmit: '提交',
      triggerRunSubmitting: '提交中…',
      triggerRunSuccess: '运行已完成',
      triggerRunError: '触发失败',
      breakActionAcknowledge: '确认',
      breakActionResolve: '标记已解决',
      breakActionIgnore: '忽略',
      breakActionReopen: '重开',
      breakResolveDialogTitle: '处理对账差异',
      breakResolveNoteLabel: '备注（写明原因，便于事后审计）',
      breakResolveSubmit: '确定',
      breakResolveSubmitting: '处理中…',
      breakDrillDownTitle: '差异明细',
      breakDetailInternalValue: '内部值',
      breakDetailBrokerValue: '券商值',
      breakDetailDiffValue: '差值',
      breakDetailDiffPercent: '差值百分比',
      breakDetailDescription: '说明',
      breakDetailMetadata: '附加信息',
      drillDownNoBreaks: '本次运行无差异',
    },
    surveillance: {
      panelTitle: '交易监控（Trade Surveillance）',
      panelSubtitle: '每小时扫描当日成交，识别 wash trade / marking close / self-trade 等可疑模式；命中即写审计链，由合规复核 cleared / escalated。',
      listEmpty: '暂无监控事件',
      listLoading: '加载中…',
      listError: '加载监控事件失败',
      refresh: '刷新',
      detectedAtLabel: '检测时间',
      ruleCodeLabel: '规则',
      severityLabel: '级别',
      statusLabel: '状态',
      symbolLabel: '标的',
      summaryLabel: '说明',
      triggerScanButton: '手动触发扫描',
      triggerScanDialogTitle: '手动触发监控扫描',
      triggerScanFundIdLabel: '基金 ID',
      triggerScanFundIdPlaceholder: '请输入要扫描的基金 UUID',
      triggerScanAsOfLabel: '业务日（YYYY-MM-DD，默认今日 UTC）',
      triggerScanSessionCloseLabel: '收盘时间 UTC（HH:MM，默认 20:00）',
      triggerScanSubmit: '提交扫描',
      triggerScanSubmitting: '扫描中…',
      triggerScanSuccess: '扫描已完成',
      triggerScanError: '扫描失败',
      severityCritical: '严重',
      severityWarning: '警告',
      severityInfo: '提示',
      statusOpen: '待复核',
      statusReviewing: '复核中',
      statusCleared: '已澄清',
      statusEscalated: '已上报',
      triggerSourceManual: '手动',
      triggerSourceScheduled: '定时调度',
      ruleWashTrade: '洗售 (wash trade)',
      ruleMarkingClose: '尾盘 marking close',
      ruleSelfTradePair: '自成交对 (self-cross)',
      ruleRapidFireReversal: '快速反向交易',
      ruleLayeringSuspect: '可疑分层下单',
      eventActionAcknowledge: '开始复核',
      eventActionClear: '澄清结案',
      eventActionEscalate: '上报合规',
      eventActionReopen: '重开复核',
      eventReviewDialogTitle: '处理监控事件',
      eventReviewNoteLabel: '复核备注（写明依据 / 上报理由，便于审计）',
      eventReviewSubmit: '确定',
      eventReviewSubmitting: '处理中…',
      eventDetailMetadata: '触发证据',
      eventDetailTradeIDs: '相关成交',
      eventDetailWindow: '检测窗口',
      runsSubpanelTitle: '最近扫描运行',
      runsTradeCountLabel: '扫描成交数',
      runsEventCountLabel: '检出事件数',
      runsDurationLabel: '耗时',
    },
    drawdown: {
      panelTitle: '回撤软熔断（Drawdown soft circuit breaker）',
      panelSubtitle:
        '按基金配置 DD 分级阈值，每 5 分钟自动评估；超阈值时记录建议降仓事件，由运维确认后通过审批流出单。auto_execute 打开的层会经审计链直接挂单（仍走风控）。',
      refresh: '刷新',
      listEmpty: '暂无回撤事件',
      listLoading: '加载中…',
      listError: '加载回撤数据失败',
      fundIdLabel: '基金 ID',
      fundIdPlaceholder: '输入基金 UUID 查看 / 配置',
      loadFundButton: '加载',
      statusTitle: '当前 DD 状态',
      peakNavLabel: '区间峰值 NAV',
      currentNavLabel: '当前 NAV',
      currentDDLabel: '当前回撤',
      hasPolicyTrue: '已配置阈值',
      hasPolicyFalse: '未配置阈值',
      breachedTierLabel: '当前已触发档位',
      triggerCheckButton: '立即检查',
      triggerCheckRunning: '检查中…',
      triggerCheckNoBreach: '未触发任何档位',
      triggerCheckBreached: '已触发：',
      triggerCheckError: '检查失败',
      tiersTitle: '阈值配置（最多 5 档，由轻到重）',
      tierLabel: '档位',
      ddPctLabel: 'DD 阈值（负数，例如 -0.05 表示 -5%）',
      actionLabel: '动作',
      trimRatioLabel: '降仓比例（trim_proportional 时生效）',
      cooldownLabel: '冷却时间（小时）',
      autoExecuteLabel: '自动执行（auto_execute）',
      noteLabel: '备注',
      addTierButton: '新增 / 修改档位',
      saveTierButton: '保存',
      saveTierSubmitting: '保存中…',
      deleteTierButton: '删除',
      deleteConfirm: '确定删除这一档？',
      actionTrimProportional: '按比例降仓',
      actionFlatten: '清仓',
      actionDefensiveOnly: '仅防守（拒绝新多头）',
      eventsTitle: '回撤事件',
      detectedAtLabel: '检测时间',
      statusLabel: '状态',
      statusProposed: '待审批',
      statusApproved: '已批准',
      statusExecuted: '已执行',
      statusDismissed: '已驳回',
      statusSuperseded: '已被覆盖',
      trimPlanTitle: '降仓计划',
      trimPlanEmpty: '本档位无具体降仓订单（defensive_only）',
      eventActionApprove: '批准并下单',
      eventActionDismiss: '驳回',
      eventActionReopen: '重开',
      reviewDialogTitle: '处理回撤事件',
      reviewNoteLabel: '备注（写明依据，便于审计）',
      reviewSubmit: '确定',
      reviewSubmitting: '处理中…',
      reviewError: '处理失败',
    },
    marketStatus: {
      panelTitle: '市场状态门控（停牌 / 涨跌停 / 陈旧报价 / 交易日历）',
      panelSubtitle:
        '在订单进入撮合引擎前先做市场可达性检查：停牌或暂停的标的、超出涨跌停的限价、陈旧报价、节假日 / 半天市等。任意硬性条件不满足则拒绝；仅警告项（如报价稍陈旧）会以备注形式跟随订单流向回放与对账。',
      refresh: '刷新',
      instrumentsTitle: '标的状态',
      instrumentsEmpty: '尚未配置任何标的',
      fieldKey: 'instrument_key',
      fieldSymbol: '代码',
      fieldMarket: '市场',
      fieldStatus: '状态',
      fieldHaltReason: '停牌原因',
      fieldHaltUntil: '停牌至',
      fieldLower: '跌停价',
      fieldUpper: '涨停价',
      fieldLastQuoteAt: '最近报价时间',
      fieldStalenessBudget: '陈旧阈值（秒）',
      statusTrading: '正常交易',
      statusHalted: '临时停牌',
      statusSuspended: '长期暂停',
      haltButton: '停牌',
      haltSubmitting: '处理中…',
      haltDialogTitle: '停牌',
      haltReasonLabel: '原因（必填）',
      haltUntilLabel: '恢复时间（可选 RFC3339）',
      unhaltButton: '复牌',
      setLimitsButton: '设置涨跌停',
      setLimitsDialogTitle: '涨跌停限价',
      upsertDialogTitle: '编辑标的状态',
      saveButton: '保存',
      saveSubmitting: '保存中…',
      cancelButton: '取消',
      eventsTitle: '门控事件',
      eventDecision: '判定',
      eventRule: '规则',
      eventSummary: '说明',
      eventDetected: '时间',
      decisionAllow: '通过',
      decisionWarn: '警告',
      decisionReject: '拒绝',
      ruleHalted: '已停牌',
      ruleSuspended: '长期暂停',
      rulePriceLimit: '涨跌停越线',
      ruleStaleQuote: '报价陈旧',
      ruleMarketClosed: '休市',
      ruleHalfDayClosed: '半天市后',
      calendarTitle: '交易日历',
      calendarMarketLabel: '市场代码（如 CN / US / HK）',
      calendarFromLabel: '从',
      calendarToLabel: '到',
      calendarLoadButton: '加载',
      calendarUpsertTitle: '新增 / 编辑日历',
      calendarIsOpen: '开市',
      calendarHalfDay: '半天市',
      calendarOpenLocal: '开市时间（HH:MM）',
      calendarCloseLocal: '收市时间（HH:MM）',
      calendarTZ: '时区',
      calendarNote: '备注',
      error: '加载失败',
    },
    marketImpact: {
      panelTitle: 'S6.2 · 大单冲击模型',
      panelSubtitle:
        '为模拟器配置每个标的的 ADV / 波动率，撮合引擎将以平方根冲击模型（bps = σ · 系数 · √(Q/ADV) · 10000）估算大单滑点，避免回测中大单仍以 last 成交、放大 P&L 的问题。未校准的标的回退到资产类别默认值。',
      refresh: '刷新',
      instrumentsTitle: '校准列表',
      instrumentsEmpty: '尚无标的校准。可点击下方"新增校准"，或保留为空走资产类别默认。',
      fieldKey: 'instrument_key',
      fieldSymbol: '代码',
      fieldMarket: '市场',
      fieldAssetClass: '资产类别',
      fieldADV: 'ADV（股 / 张）',
      fieldADVNotional: 'ADV（名义金额）',
      fieldVolatility: '日波动率 σ',
      fieldImpactCoef: '冲击系数 k',
      fieldImpactExp: '指数 α（默认 0.5）',
      fieldMinBps: '最小滑点 bps',
      fieldMaxBps: '最大滑点 bps',
      fieldLastCalibrated: '最近校准时间',
      fieldSource: '校准来源',
      upsertButton: '新增 / 编辑',
      upsertDialogTitle: '编辑校准',
      deleteButton: '删除',
      deleteConfirm: '确认删除此标的的校准？删除后将回退到资产类别默认值。',
      saveButton: '保存',
      saveSubmitting: '保存中…',
      cancelButton: '取消',
      sourceManual: '手工录入',
      sourceHistorical: '历史回算',
      sourceBrokerReported: '券商上报',
      previewTitle: '撮合冲击预演',
      previewSubtitle: '不下单，仅基于当前校准估算一笔订单的滑点 bps 与隐含成交价。',
      previewSide: '方向',
      previewSideBuy: '买入',
      previewSideSell: '卖出',
      previewQuantity: '数量',
      previewReferencePrice: '参考价格',
      previewSubmit: '运行预演',
      previewSubmitting: '运行中…',
      previewResult: '预演结果',
      previewBps: '不利滑点',
      previewImpliedFill: '隐含成交价',
      previewImpactCost: '冲击成本（参考币）',
      previewUsedDefaults: '使用资产类别默认',
      previewUsedADVFallback: 'ADV 缺失，回退到 min_bps',
      cacheTitle: '内存缓存',
      cacheSize: '校准条目',
      cacheLastRefresh: '最近刷新',
      cacheRefreshButton: '强制刷新',
      cacheRefreshing: '刷新中…',
      error: '加载失败',
    },
    lockup: {
      panelTitle: 'S6.3 · IPO / 受限股 lock-up',
      panelSubtitle:
        '为 IPO 配售、定增、RSU、限售股等受限持仓登记锁定期。撮合时若 SELL 数量超过 (持仓 - 活跃锁定 qty)，模拟器会拒单。可在到期前手工提前释放，操作会写入审计。',
      refresh: '刷新',
      listTitle: 'Lock-up 记录',
      listEmpty: '暂无 lock-up 记录',
      fieldFund: '基金',
      fieldInstrument: 'instrument_key',
      fieldSymbol: '代码',
      fieldQty: '锁定数量',
      fieldUntil: '锁定至',
      fieldReason: '原因',
      fieldNote: '备注',
      fieldStatus: '状态',
      fieldSourceLot: '关联 lot',
      fieldReleasedAt: '提前释放时间',
      fieldReleasedReason: '释放原因',
      statusActive: '生效中',
      statusExpired: '已到期',
      statusReleased: '已提前释放',
      reasonIPO: 'IPO 配售',
      reasonPrivatePlacement: '定向增发',
      reasonRSU: 'RSU',
      reasonRestricted: '限售股',
      reasonEmployeeGrant: '员工股权',
      reasonBlockSale: '大宗交易锁定',
      reasonOther: '其他',
      filterAll: '全部',
      createButton: '新增',
      createDialogTitle: '登记 Lock-up',
      editButton: '编辑',
      editDialogTitle: '编辑 Lock-up',
      deleteButton: '删除',
      deleteConfirm: '直接删除会丢失审计痕迹，确认要删除而不是「提前释放」吗？',
      releaseButton: '提前释放',
      releaseDialogTitle: '提前释放 Lock-up',
      releaseReasonLabel: '释放原因（必填，会写入审计日志）',
      saveButton: '保存',
      saveSubmitting: '保存中…',
      cancelButton: '取消',
      error: '加载失败',
    },
    borrow: {
      panelTitle: 'S6.4 · 借券与 locate 费',
      panelSubtitle:
        '为可融券品种登记借券费率、locate 费、可用数量。模拟器在 SHORT 开仓时按需走 locate gate；EOD 自动按持仓 × 当日收盘价 × 年化费率 / 365 计提借券费，写入 cash_ledger（borrow_fee）+ 短仓借券台账。',
      refresh: '刷新',
      listTitle: '借券费率',
      listEmpty: '暂未登记任何借券费率',
      fieldKey: 'instrument_key',
      fieldSymbol: '代码',
      fieldMarket: '市场',
      fieldRate: '年化费率 (bps)',
      fieldLocateFee: 'Locate 费 (bps)',
      fieldAvailability: '可借状态',
      fieldAvailable: '可借数量',
      fieldMinLocate: 'locate 最小',
      fieldMaxLocate: 'locate 最大',
      fieldSource: '来源',
      fieldNote: '备注',
      availEasy: '易借',
      availHard: '难借',
      availRestricted: '受限',
      availUnavailable: '不可借',
      sourceManual: '手动登记',
      sourceBrokerQuote: '券商报价',
      sourceAgentLender: 'Agent lender',
      sourceHistorical: '历史校准',
      sourcePublicFeed: '公开数据',
      upsertButton: '保存',
      upsertSubmitting: '保存中…',
      deleteButton: '删除',
      cacheTitle: '内存缓存',
      cacheSize: '缓存条目数',
      cacheLastRefresh: '上次刷新',
      cacheRefreshButton: '强制刷新',
      cacheRefreshing: '刷新中…',
      previewTitle: 'Locate 预演',
      previewSubtitle:
        '不下单的情况下试算 locate gate 的判定结果（含 locate 费）。',
      previewFundLabel: '基金 ID',
      previewKeyLabel: 'instrument_key',
      previewQtyLabel: '请求数量',
      previewPriceLabel: '预期价格',
      previewSubmit: '预演',
      previewSubmitting: '计算中…',
      previewResultDecision: '判定',
      previewResultRate: '年化借券费率 (bps)',
      previewResultLocateFee: 'Locate 费',
      previewResultNotional: 'Notional',
      auditTitle: 'Locate 审计日志',
      auditFundFilter: '按基金过滤',
      auditDecisionFilter: '按判定过滤',
      auditEmpty: '暂无 locate 审计记录',
      ledgerTitle: '借券费台账',
      ledgerEmpty: '暂无借券费记录',
      error: '加载失败',
    },
    wsfeed: {
      panelTitle: 'S6.5 · WebSocket 实时行情',
      panelSubtitle:
        '把 broker 撮合 + 持仓刷新的报价来源从 REST 轮询换成 push tick。配置 WSFEED_ENABLED=true 并配上 provider（mock / 真实券商）后，所有热路径优先读 WS-cache，cache miss 或 stale 时自动回退 REST。',
      disabled: '当前禁用：',
      refresh: '刷新',
      reconcile: '立即对齐订阅',
      reconcileSubmitting: '对齐中…',
      statusEnabled: '运行中',
      statusHealthyProviders: '健康 provider',
      statusSubscriptions: '订阅数',
      statusCacheSymbols: '缓存合约',
      statusTotalTicks: '累计 tick 数',
      statusDroppedEvents: '丢弃事件',
      connectionsTitle: '上游连接',
      connectionsEmpty: '未注册任何 provider',
      colProvider: 'Provider',
      colState: '状态',
      colTickCount: 'Tick 数',
      colReconnects: '重连',
      colLastTick: '最近 tick',
      colConnectedAt: '连接时间',
      colLastError: '最近错误',
      stateConnected: '已连接',
      stateConnecting: '连接中',
      stateReconnecting: '重连中',
      stateBackoff: '退避中',
      stateDisconnected: '断开',
      stateClosed: '已关闭',
      stateUnknown: '未知',
      subscriptionsTitle: '当前订阅',
      subscriptionsEmpty: '当前没有任何订阅',
      colSymbol: '合约',
      colMarket: '市场',
      colConsumers: '订阅方',
      cacheTitle: 'Quote Cache',
      cacheStats: '命中 / 错失 / 过期 / 淘汰',
      cacheEmpty: '缓存为空',
      colLast: '最新',
      colBid: 'Bid',
      colAsk: 'Ask',
      colAsOf: '时间',
      colStale: '过期',
      subscribeTitle: '手动订阅',
      subscribeSymbolPlaceholder: '合约（如 AAPL）',
      subscribeMarketPlaceholder: '市场（如 US）',
      subscribeSubmit: '订阅',
      subscribeSubmitting: '提交中…',
      unsubscribeButton: '退订',
      evictCacheTitle: '缓存清理',
      evictCacheButton: '清理本行',
      evictCacheAllButton: '清空缓存',
      error: '加载失败',
    },
    factorExposure: {
      panelTitle: '因子敞口',
      panelSubtitle: '当前持仓在 size / value / momentum / quality / lowvol / market_beta 六个标准因子上的净敞口与总敞口，以及调用时的覆盖率。',
      refresh: '刷新',
      loading: '计算中…',
      empty: '当前无持仓',
      error: '加载失败',
      navLabel: '总市值',
      holdingsLabel: '持仓数',
      coverageLabel: '覆盖率',
      loadingsAsOfLabel: '校准日',
      loadingsAsOfStale: '校准已过期',
      factorSize: 'Size',
      factorValue: 'Value',
      factorMomentum: 'Momentum',
      factorQuality: 'Quality',
      factorLowVol: 'Low Vol',
      factorMarketBeta: 'Market β',
      netExposureLabel: '净敞口',
      grossExposureLabel: '总敞口',
      holdingCountLabel: '贡献持仓',
      trendTitle: '30 天趋势',
      trendEmpty: '尚无历史快照',
      adminPanelTitle: '因子载荷管理',
      adminPanelSubtitle: '维护 instrument_factor_loadings 表。校准来源 manual / msci / eastmoney / computed / override 分别对应人工录入、第三方供应、Quant Lab 计算与紧急覆写。',
      adminListTitle: '当前校准记录',
      adminListEmpty: '暂无校准记录',
      adminInstrumentKey: '标的 Key',
      adminFactorLabel: '因子',
      adminAsOfLabel: '校准日',
      adminLoadingLabel: '载荷',
      adminSourceLabel: '来源',
      adminNoteLabel: '备注',
      adminUpdatedAtLabel: '更新时间',
      adminFactorAll: '全部因子',
      adminUpsertTitle: '新增 / 更新载荷',
      adminUpsertSubmit: '保存',
      adminUpsertSubmitting: '保存中…',
      adminDeleteButton: '删除',
      adminDeleteConfirm: '确认删除这条载荷记录？',
      sourceManual: '人工',
      sourceEastMoney: '东方财富',
      sourceMSCI: 'MSCI',
      sourceComputed: 'Quant Lab',
      sourceOverride: '紧急覆写',
    },
    varRisk: {
      panelTitle: '风险价值 (VaR / CVaR)',
      panelSubtitle: '基于 nav_snapshots.daily_return 时序，按三种方法（历史模拟 / 参数法 / 蒙特卡洛）与三档置信度（90% / 95% / 99%）计算单期最大可能损失。',
      refresh: '刷新',
      loading: '计算中…',
      empty: '暂无数据',
      error: '加载失败',
      insufficientHistory: '历史样本不足，请先累积至少 20 个交易日的 NAV 序列。',
      sampleSizeLabel: '样本数',
      lookbackLabel: '回看天数',
      horizonLabel: '持有期',
      horizon1d: '1 日',
      horizon5d: '5 日',
      horizon10d: '10 日',
      meanLabel: '日均收益',
      stdevLabel: '波动率',
      sampleWindowLabel: '样本区间',
      methodLabel: '方法',
      confidenceLabel: '置信度',
      varLabel: 'VaR',
      cvarLabel: 'CVaR',
      methodHistorical: '历史模拟',
      methodParametric: '参数法',
      methodMonteCarlo: '蒙特卡洛',
      methodHistoricalSubtitle: '不假设分布，对收益序列直接取分位数',
      methodParametricSubtitle: '假设正态分布，按 μ − z·σ 闭式计算',
      methodMonteCarloSubtitle: '从 N(μ, σ) 抽样 5 万次后取分位数',
      confidence90Label: '90%',
      confidence95Label: '95%',
      confidence99Label: '99%',
      varInterpretation: '在该置信度下，下一个持有期内损失大概率不超过此值',
      cvarInterpretation: '在 VaR 被突破的情况下，预期损失的平均值（尾部期望）',
    },
    stressTest: {
      panelTitle: '压力测试',
      panelSubtitle: '从管理员维护的场景库中选择一个（历史复刻 / 假设情景 / 监管标准），把对应的冲击应用到当前持仓上，看 NAV 在该情景下的预计变动。',
      runButton: '运行场景',
      running: '运行中…',
      refresh: '刷新',
      empty: '请选择一个场景以查看影响',
      error: '运行失败',
      scenarioLabel: '场景',
      scenarioPlaceholder: '选择压力情景…',
      categoryLabel: '类别',
      descriptionLabel: '说明',
      shockCountLabel: '冲击数',
      navBeforeLabel: '冲击前 NAV',
      navAfterLabel: '冲击后 NAV',
      pnlTotalLabel: '损益合计',
      pnlPctLabel: '损益占比',
      holdingsLabel: '持仓数',
      shockedLabel: '受冲击持仓',
      impactsTitle: '持仓级影响',
      impactsEmpty: '暂无持仓影响',
      impactSymbol: '代码',
      impactBefore: '冲击前',
      impactAfter: '冲击后',
      impactPnL: '损益',
      impactReturn: '冲击收益率',
      impactShock: '匹配冲击',
      categoryHistorical: '历史复刻',
      categoryHypothetical: '假设情景',
      categoryRegulatory: '监管标准',
      adminPanelTitle: '压力情景库',
      adminPanelSubtitle: '维护 stress_scenarios 表。情景定义里的 shock 数组按 instrument > market > asset_class > factor > wildcard 的特异性匹配持仓；factor 类冲击会与 instrument_factor_loadings 复合相加。',
      adminListTitle: '当前情景',
      adminListEmpty: '暂无情景，请新增',
      adminScenarioName: '名称',
      adminScenarioCategory: '类别',
      adminScenarioDescription: '说明',
      adminScenarioShocks: '冲击列表',
      adminScenarioCreatedBy: '创建人',
      adminScenarioUpdatedAt: '更新时间',
      adminUpsertTitle: '新增 / 更新场景',
      adminUpsertSubmit: '保存',
      adminUpsertSubmitting: '保存中…',
      adminDeleteButton: '删除',
      adminDeleteConfirm: '确认删除该场景？删除会级联清理历史 stress 结果。',
      targetInstrument: '标的级',
      targetMarket: '市场级',
      targetAssetClass: '资产类别级',
      targetFactor: '因子级',
      targetWildcard: '通配（全部持仓）',
    },
    brinsonAttribution: {
      panelTitle: 'Brinson 业绩归因',
      panelSubtitle: '将组合相对基准的超额收益拆解为配置效应、选股效应和交互效应。先在管理员后台维护基准成分（每个分桶的权重和收益），再在此面板按资产类别 / 市场维度运行归因。',
      runButton: '运行归因',
      running: '计算中…',
      benchmarkLabel: '基准',
      benchmarkPlaceholder: '选择基准…',
      dimensionLabel: '分桶维度',
      dimensionAssetClass: '资产类别',
      dimensionMarket: '市场',
      dimensionSector: '行业（暂未支持）',
      benchmarkEmpty: '尚未配置基准成分，请联系管理员到 brinson_benchmark_compositions 后台添加',
      portfolioReturn: '组合收益',
      benchmarkReturn: '基准收益',
      activeReturn: '主动收益',
      allocationEffect: '配置效应',
      selectionEffect: '选股效应',
      interactionEffect: '交互效应',
      totalEffect: '合计效应',
      decompositionTitle: '三效应分解',
      bucketsTitle: '分桶明细',
      bucketsEmpty: '暂无分桶明细',
      colBucket: '分桶',
      colPortfolioWeight: '组合权重',
      colBenchmarkWeight: '基准权重',
      colPortfolioReturn: '组合收益',
      colBenchmarkReturn: '基准收益',
      colAllocation: '配置',
      colSelection: '选股',
      colInteraction: '交互',
      colTotal: '合计',
      persistLabel: '存档此次归因',
      error: '归因运行失败',
      noPortfolioHoldings: '当前持仓在所选维度上没有有效分类',
      compositionNotFound: '未找到该基准的成分数据',
      sectorUnsupported: '行业维度需要先维护持仓的行业分类，暂未支持',
      asofLabel: '截至日期',
      adminPanelTitle: 'Brinson 基准成分库',
      adminPanelSubtitle: '维护 brinson_benchmark_compositions 表。每个 (benchmark_id, dimension, asof) 一行，buckets JSONB 数组里每个分桶 (key, weight, return_pct) 表示基准在该桶里的权重和当期收益。',
      adminListTitle: '当前成分',
      adminListEmpty: '暂无基准成分，请新增',
      adminUpsertTitle: '新增 / 更新基准成分',
      adminUpsertSubmit: '保存',
      adminUpsertSubmitting: '保存中…',
      adminDeleteButton: '删除',
      adminDeleteConfirm: '确认删除该基准成分？会级联清理引用它的归因快照。',
      adminBucketKey: '分桶 Key',
      adminBucketWeight: '权重 (0-1)',
      adminBucketReturn: '收益 (e.g. 0.05)',
      adminAddBucket: '+ 添加分桶',
      adminRemoveBucket: '删除',
      adminBenchmarkId: '基准 ID',
      adminAsof: '截至日期',
      adminNote: '备注',
    },
    analystPanel: {
      title: '分析师面板',
      subtitle: '基本面 / 情绪 / 新闻 / 技术四位专业化分析师独立给出结论，面板按各自置信度加权得出整体判断。每位分析师都基于规则给出确定性的方向锚点，LLM 仅在叙述层加成。',
      symbolLabel: '标的代码',
      symbolPlaceholder: '如 AAPL / 600519',
      runButton: '运行分析师面板',
      running: '4 位分析师并行打分中…',
      persistLabel: '存档此次面板',
      aggregateTitle: '综合判断',
      aggregateDirection: '方向',
      aggregateConfidence: '置信度',
      categoriesVoted: '参与表态分析师数',
      voteSummary: '{voted}/{total} 位分析师明确表态',
      perCategoryTitle: '各分析师报告',
      asof: '截至',
      generatedAt: '生成于',
      directionBullish: '看多',
      directionBearish: '看空',
      directionNeutral: '中性',
      categoryFundamentals: '基本面',
      categorySentiment: '情绪',
      categoryNews: '新闻 / 催化',
      categoryTechnical: '技术面',
      thesisLabel: '论点',
      keyFindingsLabel: '关键发现',
      risksLabel: '风险点',
      dataPointsLabel: '数据指标',
      sourcesLabel: '信息源',
      noPanelYet: '尚未运行分析师面板',
      error: '面板运行失败',
      historyTitle: '历史面板',
      historyEmpty: '暂无历史面板',
      historyLoading: '加载中…',
      confidenceLabel: '置信度 {value}%',
      llmModelFallback: '规则回退',
      llmModelLLM: 'LLM',
    },
    bullBearDebate: {
      title: '多空对辩',
      subtitle: '基于分析师面板的结论，强制让 Bull / Bear 两位研究员各执一词、交替反驳。Bull 必须找出最强买入理由，Bear 必须找出最强卖出 / 回避理由，谁也不能中立。回合越深，反驳越精准，PM 在最后只读对辩结论。',
      symbolLabel: '标的代码',
      symbolPlaceholder: '如 AAPL / 600519',
      roundsLabel: '辩论轮数',
      runButton: '运行多空对辩',
      running: '多空研究员对辩中…',
      notesLabel: '备注',
      verdictTitle: '对辩裁决',
      verdictDirection: '方向',
      verdictConfidence: '置信度',
      verdictContested: '势均力敌（多空差距 < 20%）',
      verdictNotContested: '分差明显',
      bullConfidence: '多头平均置信度',
      bearConfidence: '空头平均置信度',
      winnerBull: '多头胜出',
      winnerBear: '空头胜出',
      winnerTie: '平局',
      argumentsTitle: '逐轮发言',
      roundLabel: '第 {round} 轮',
      stanceBull: '多头',
      stanceBear: '空头',
      thesisLabel: '论点',
      supportPointsLabel: '支撑证据',
      rebuttalsLabel: '反驳对手',
      citedReportsLabel: '引用分析师',
      noDebateYet: '尚未运行对辩',
      error: '对辩运行失败',
      historyTitle: '历史对辩',
      historyEmpty: '暂无历史对辩',
      historyLoading: '加载中…',
      confidenceLabel: '置信度 {value}%',
      llmModelFallback: '规则回退',
      llmModelLLM: 'LLM',
    },
    corpActions: {
      title: '分红 · 拆股 · 配股记录',
      subtitle: '近期发生在持仓上的公司行动事件',
      expand: '展开',
      collapse: '收起',
      loading: '加载中…',
      error: '加载失败',
      retry: '重试',
      empty: '近期无公司行动事件',
      typeSplit: '拆股 / 送股',
      typeCashDividend: '现金分红',
      typeStockDividend: '送股转增',
      typeCombined: '派股 + 派现',
      sharesLabel: '份额',
      costLabel: '成本',
      cashLabel: '现金到账',
      exDateLabel: '除权日',
    },
    benchmark: {
      title: '基金 vs 大盘',
      subtitle: '净值与基准指数同起点归一化（起始 = 100）',
      fund: '本基金',
      days7: '7 天',
      days30: '30 天',
      days90: '90 天',
      days180: '180 天',
      days365: '1 年',
      expand: '展开',
      collapse: '收起',
      loading: '加载中…',
      empty: '暂无可对比的净值数据',
      error: '基准加载失败',
      retry: '重试',
      seriesPicker: '基准指数',
      addSeries: '添加基准',
      partialFailureToast: '部分基准未能加载，已跳过',
      legendStart: '起始 = 100',
      holdingOverlapDominantTitle: '本基金主仓 ≈ 大盘',
      holdingOverlapDominantBody: '基金主要持仓与所选基准为同一标的，"对比"模式下两条曲线会高度重合；建议切换到 Alpha 视图查看相对超额收益。',
      holdingOverlapPartialTitle: '部分持仓与基准重叠',
      holdingOverlapPartialBody: '基金部分持仓与所选基准为同一标的，"对比"视图可能不直观，可切到 Alpha 视图观察相对走势。',
      holdingOverlapSwitchToAlpha: '切换到 Alpha 视图',
    },
    holdingsSeries: {
      title: '持仓走势',
      subtitle: '每只持仓在该窗口内的归一化股价（起始 = 100）',
      expand: '展开',
      collapse: '收起',
      loading: '加载中…',
      error: '走势加载失败',
      retry: '重试',
      empty: '暂无可绘制的持仓',
      vsEntry: '相对成本',
      vsStart: '相对窗口起点',
      partialFailureToast: '以下持仓未能加载',
      days30: '30 天',
      days90: '90 天',
      days180: '180 天',
    },
    abShadow: {
      sectionTitle: '影子 Agent 对比',
      sectionSubtitle: '查看 A 组与 B 组每位 agent 在影子运行中学到的内容、调整建议与提议的演化配置差异',
      expand: '展开',
      collapse: '收起',
      loading: '加载影子 agent 数据…',
      error: '加载影子 agent 数据失败',
      retry: '重试',
      empty: '该测试暂无影子 agent 学习数据',
      notAnalyzedYet: '完成"生成分析"后即可查看 A vs B 影子 agent 学习对比',
      columnA: 'A 组',
      columnB: 'B 组',
      eventCount: '学习事件数',
      latestDate: '最新事件日期',
      lessons: '关键经验',
      adjustments: '建议调整',
      summaries: '近期总结',
      timeline: '逐日时间线',
      memories: '影子记忆',
      proposedDiff: '提议的 evolution_config 变更',
      diffAdded: '新增',
      diffChanged: '变更（旧 → 新）',
      diffRemoved: '移除',
      noDiff: '与当前 evolution_config 一致，无需变更',
      deterministicShadowBanner: '当前 B 组采用确定性影子执行策略，数据用于策略参数 sanity check；后续 Card K 将引入真实 LLM 影子运行。',
    },
    abAttribution: {
      sectionTitle: '按标的归因',
      sectionSubtitle: '比较 A vs B 在每只标的上的成交、成本与盈亏差异',
      expand: '展开',
      collapse: '收起',
      loading: '加载归因数据…',
      error: '加载归因数据失败',
      retry: '重试',
      empty: '该测试暂无影子交易归因数据',
      columnSymbol: '标的',
      columnTradesA: 'A 笔数',
      columnTradesB: 'B 笔数',
      columnPnLA: 'A 已实现盈亏',
      columnPnLB: 'B 已实现盈亏',
      columnTurnoverA: 'A 成交额',
      columnTurnoverB: 'B 成交额',
      columnGap: '差额（B − A）',
      columnGapPct: '差额占成交额',
      columnWinner: '胜出版本',
      winnerA: 'A',
      winnerB: 'B',
      winnerTie: '持平',
      totalsTitle: '总览',
      avgPnL: '平均盈亏',
      winRate: '盈利交易占比',
    },
  },
  'en-US': {
    auth: {
      loginTitle: 'Sign in',
      email: 'Email',
      password: 'Password',
      submit: 'Sign in',
      submitting: 'Signing in…',
      forgot: 'Forgot password?',
      errorInvalid: 'Invalid email or password',
      errorGeneric: 'Sign-in failed. Please try again later.',
      biometricsPrompt: 'Unlock with biometrics',
      biometricsRequired: 'Biometric authentication failed',
      biometricsBlockedHint: 'Biometrics failed or were cancelled. Please sign in again with your password.',
      sessionErrorTitle: 'Cannot reach the service',
      sessionErrorHint: 'A network or backend issue is blocking your session. Retry or sign in again.',
      sessionErrorRetry: 'Retry connection',
      forgotTitle: 'Reset password',
      forgotHint: 'We will send a link to that email.',
      forgotSubmit: 'Send email',
      forgotSent: 'Sent — check your inbox.',
      backToLogin: 'Back to sign-in',
      resetTitle: 'Set a new password',
      resetHint: 'Use at least 8 characters. You will need to sign in again afterwards.',
      resetNewPassword: 'New password',
      resetConfirmPassword: 'Confirm new password',
      resetSubmit: 'Update password',
      resetSubmitting: 'Updating…',
      resetSuccess: 'Password updated — please sign in again.',
      resetTokenInvalid: 'This link is invalid or expired. Please request a new one.',
      resetPasswordMismatch: 'The two passwords do not match.',
      twoFATitle: 'Two-factor verification',
      twoFASubtitle: 'Enter the 6-digit code from your authenticator app.',
      twoFAModeCode: 'Authenticator code',
      twoFAModeRecovery: 'Recovery code',
      twoFACodePlaceholder: '6-digit code',
      twoFARecoveryPlaceholder: 'Recovery code',
      twoFASubmit: 'Verify and continue',
      twoFACancel: 'Use a different account',
      twoFAInvalidCode: 'Invalid code, please try again.',
    },
    tabs: { home: 'Home', decisions: 'Decisions', memory: 'Memory', team: 'Team', more: 'More', orders: 'Orders' },
    home: {
      title: 'My funds',
      empty: 'No funds yet; create one in the web app.',
      loading: 'Loading…',
      error: 'Failed to load',
      retry: 'Retry',
      navLabel: 'NAV',
      assetsLabel: 'Assets',
    },
    decisions: {
      title: 'Latest decisions',
      empty: 'No plans generated today.',
      loadFailed: 'Failed to load decisions. Please retry.',
      retry: 'Retry',
      actionsLabel: 'actions',
      approve: 'Approve plan',
      reject: 'Reject plan',
      refresh: 'Refresh quote',
      approving: 'Approving…',
      rejecting: 'Rejecting…',
      refreshing: 'Refreshing…',
      rejectReasonPrompt: 'Briefly describe the reason (1–200 chars)',
      rejectReasonRequired: 'A reason is required to reject',
      confirm: 'Confirm',
      cancel: 'Cancel',
      successApproved: 'Approved — queued for execution',
      successRejected: 'Plan rejected',
      successRefreshed: 'Quote refreshed',
      actionFailed: 'Action failed. Please retry.',
      statusDraft: 'Draft',
      statusRiskReview: 'Risk review',
      statusPendingUser: 'Awaiting approval',
      statusApproved: 'Approved',
      statusRejected: 'Rejected',
      statusExecuting: 'Executing',
      statusCompleted: 'Completed',
      statusFailed: 'Failed',
      statusMixed: 'Partially filled',
      recentEvents: 'Recent events',
    },
    memory: {
      title: 'Memory & reflections',
      tabs: { agent: 'Daily learning', reflection: 'Long-term reflections' },
      empty: 'No memories yet.',
      error: 'Failed to load',
      retry: 'Retry',
    },
    team: { title: 'Agent team', empty: 'No agents configured for this fund.', error: 'Failed to load', retry: 'Retry' },
    orders: {
      title: 'My orders',
      empty: 'No open orders.',
      loadFailed: 'Failed to load orders',
      retry: 'Retry',
      actionsLabel: 'Actions',
      cancel: 'Cancel',
      replace: 'Modify',
      cancelling: 'Cancelling…',
      replacing: 'Saving…',
      cancelConfirmTitle: 'Cancel order',
      cancelConfirmBody: 'Cancel this order? It will be recorded in the audit log and cannot be undone.',
      cancelOk: 'Confirm',
      cancelOkConfirm: 'Cancel order',
      cancelDismiss: 'Dismiss',
      cancelSuccess: 'Order cancelled.',
      replaceTitle: 'Modify order',
      replaceQuantity: 'New quantity',
      replaceLimit: 'New limit price',
      replaceStop: 'New stop trigger',
      replaceTrailAmount: 'New trail amount',
      replaceTrailPercent: 'New trail percent (0-1)',
      replaceDisplayQty: 'New display qty (iceberg)',
      replaceNote: 'Reason (optional)',
      replaceLeaveBlankHint: 'Leave blank to keep the current value.',
      replaceSubmit: 'Save changes',
      replaceCancel: 'Cancel',
      replaceSuccess: 'Order updated.',
      actionFailed: 'Action failed',
      stepUpCancelReason: 'Confirm cancel with biometrics',
      stepUpReplaceReason: 'Confirm replace with biometrics',
      liveBannerTitle: 'Live trading prerequisites',
      liveBannerSubtitle: 'This fund is in live mode. All four checks must pass before placing, replacing, or cancelling orders.',
      liveBannerEnforced: 'Hard gate enabled',
      liveBannerBypass: 'Hard gate off (dev mode)',
      livePillarKYC: 'KYC verification',
      livePillarBrokerLink: 'Broker account link',
      livePillarTwoFA: '2FA / TOTP',
      livePillarStepUp: 'Biometric confirmation',
      livePillarOK: 'Passed',
      livePillarMissing: 'Action required',
      liveBlockedKYC: 'Please complete KYC verification first',
      liveBlockedBrokerLink: 'Please link a broker account first',
      liveBlockedTwoFA: 'Please enable 2FA / TOTP first',
      liveBlockedStepUp: 'Please confirm with biometrics first',
      columns: { symbol: 'Symbol', side: 'Side', qty: 'Qty', price: 'Price', status: 'Status' },
    },
    more: {
      title: 'More',
      language: 'Language',
      logout: 'Sign out',
      version: 'Version',
      darkMode: 'Appearance',
      appearanceSystem: 'System',
      appearanceLight: 'Light',
      appearanceDark: 'Dark',
      accountSecurity: 'Account & security',
      accountInfoLabel: 'Signed in as',
      accountInfoMissing: 'Account info unavailable',
      accountEmailVerifiedOn: 'Email verified',
      accountEmailVerifiedOff: 'Email pending verification',
      changePassword: 'Change password',
      biometric: 'Biometric unlock',
      biometricOn: 'Enabled',
      biometricOff: 'Disabled',
      biometricHint: 'When off, the app opens directly without a biometric prompt.',
      biometricUnavailable: 'No fingerprint / Face ID enrolled on this device',
      notifications: 'Push notifications',
      notificationsOn: 'On',
      notificationsOff: 'Off',
      notificationsHint: 'Notified on plan ready, risk anomalies and reflection updates.',
      notificationsRegistering: 'Registering for push…',
      notificationsRegistrationFailed: 'Push registration failed. Please retry.',
      sectionAccount: 'Account',
      sectionAppearance: 'Appearance',
      sectionLanguage: 'Language',
      sectionDanger: 'Session',
      recentEvents: 'Recent events',
      twoFATitle: 'Two-factor authentication',
      twoFAHintLoading: 'Loading status…',
      twoFAHintEnabled: 'Enabled. Manage in the web account security page.',
      twoFAHintDisabled: 'Not enabled. Set it up in the web app.',
      twoFAStatusOn: 'On',
      twoFAStatusOff: 'Off',
      stepUpOrders: 'Biometric for orders',
      stepUpOrdersHint: 'When on, every cancel / replace asks for biometric confirmation.',
    },
    brokerLinks: {
      title: 'Broker account link',
      subtitle:
        'Link an external broker account to this fund. New requests start as pending and require a different super_admin to approve (4-eye check) before live trading is unlocked.',
      formTitle: 'Submit a new link request',
      formBroker: 'Broker',
      formAccountId: 'Broker account ID',
      formAccountIdPlaceholder: 'e.g. U1234567',
      formSubmit: 'Submit request',
      formSubmitting: 'Submitting…',
      formNote:
        'After submitting, wait for an admin 4-eye approval. Only approved links count toward the live-trading gate.',
      refresh: 'Refresh',
      empty: 'No broker links yet',
      loading: 'Loading…',
      revoke: 'Revoke',
      revoking: 'Revoking…',
      confirmRevoke:
        'Revoking will immediately block live cancel/replace until a new link is approved. Continue?',
      statusPending: 'Pending approval',
      statusActive: 'Active',
      statusSuspended: 'Suspended',
      statusRevoked: 'Revoked',
      errorPrefix: 'Action failed: ',
    },
    funding: {
      title: 'Deposits & withdrawals',
      subtitle:
        'Submit a deposit or withdrawal request. The amount only posts to cash_ledger after a different super_admin approves (4-eye).',
      formTitle: 'New funding request',
      formDirection: 'Direction',
      formDirectionDeposit: 'Deposit',
      formDirectionWithdrawal: 'Withdrawal',
      formAmount: 'Amount',
      formAmountPlaceholder: 'e.g. 100000',
      formCurrency: 'Currency',
      formMethod: 'Method',
      formExternalReference: 'External reference',
      formExternalReferencePlaceholder: 'e.g. wire ref or ACH trace id',
      formNotes: 'Notes',
      formNotesPlaceholder: 'Ticket number or context for the approver',
      formSubmit: 'Submit request',
      formSubmitting: 'Submitting…',
      formNote: 'Withdrawals are checked against current_capital at approval time and rejected if insufficient.',
      methodWire: 'Wire',
      methodACH: 'ACH',
      methodSEPA: 'SEPA',
      methodCheck: 'Check',
      methodInternal: 'Internal transfer',
      methodManual: 'Manual',
      refresh: 'Refresh',
      empty: 'No funding requests yet',
      loading: 'Loading…',
      cancel: 'Cancel',
      cancelling: 'Cancelling…',
      confirmCancel:
        'Cancelling removes this request from the approval queue. You can submit a new one. Continue?',
      statusPending: 'Pending approval',
      statusApproved: 'Approved',
      statusRejected: 'Rejected',
      statusCancelled: 'Cancelled',
      statusPosted: 'Posted',
      rejectionReasonLabel: 'Reason',
      awaitingApproval: 'Awaiting admin 4-eye approval',
      errorPrefix: 'Action failed: ',
      insufficientCash: 'Insufficient cash to approve this withdrawal',
    },
    fx: {
      panelTitle: 'FX rates',
      panelSubtitle:
        'The platform fetches USD-anchored pairs from Yahoo every 6 hours; operators can override here. NAV and cash-ledger summaries fall back to the most recent manual / override / yahoo row when converting into the fund\'s base_currency.',
      listEmpty: 'No FX rates recorded yet.',
      listLoading: 'Loading…',
      listError: 'Failed to load FX rates',
      refresh: 'Refresh',
      pairLabel: 'Pair',
      rateLabel: 'Rate',
      rateAtLabel: 'Observed at',
      sourceLabel: 'Source',
      formTitle: 'Manual override',
      formBase: 'Base',
      formQuote: 'Quote',
      formRate: 'Rate (1 base = ? quote)',
      formRatePlaceholder: 'e.g. 7.18',
      formSource: 'Source label',
      formSourceManual: 'manual (operator entered)',
      formSourceOverride: 'override (replaces a wrong auto fetch)',
      formNote: 'Note (optional)',
      formNotePlaceholder: 'Explain the override; lands in the audit chain.',
      formSubmit: 'Submit',
      formSubmitting: 'Submitting…',
      formSuccess: 'Wrote fx_rates and audit log.',
      sourceManual: 'manual',
      sourceOverride: 'override',
      sourceYahoo: 'Yahoo',
      sourceEod: 'EOD',
      fundBaseCurrencyLabel: 'Reporting currency',
      fundBaseCurrencyHint:
        'Switching to a non-USD base will convert every position and cash bucket via the latest USD-anchored rate before showing NAV.',
      fundBaseCurrencySaving: 'Saving…',
      fundBaseCurrencySaved: 'Saved.',
      fxStaleBanner:
        'Some currencies have no recent FX rate; balances are counted at face value as a fallback. Treat totals as approximate.',
    },
    recon: {
      panelTitle: 'Daily reconciliation',
      panelSubtitle:
        'The platform diffs internal positions / cash / trades against a (mock) broker statement nightly. Breaks land on the audit chain — operators acknowledge or resolve from this panel.',
      listEmpty: 'No reconciliation runs yet.',
      listLoading: 'Loading…',
      listError: 'Failed to load reconciliation runs',
      refresh: 'Refresh',
      runDateLabel: 'As-of',
      triggerSourceLabel: 'Trigger',
      statusLabel: 'Status',
      breakCountLabel: 'Breaks',
      breakCountCriticalLabel: 'Critical',
      breakCountWarningLabel: 'Warning',
      breakCountInfoLabel: 'Info',
      severityCritical: 'critical',
      severityWarning: 'warning',
      severityInfo: 'info',
      statusOpen: 'open',
      statusAcknowledged: 'acknowledged',
      statusResolved: 'resolved',
      statusIgnored: 'ignored',
      statusPending: 'running',
      statusCompleted: 'completed',
      statusFailed: 'failed',
      triggerSourceManual: 'manual',
      triggerSourceScheduled: 'scheduled',
      triggerSourceReplay: 'replay',
      breakTypePositionQuantity: 'Position quantity mismatch',
      breakTypePositionAvgCost: 'Position avg-cost mismatch',
      breakTypePositionMissingInternal: 'Position missing internally',
      breakTypePositionMissingBroker: 'Position missing on broker',
      breakTypeCashBalance: 'Cash balance mismatch',
      breakTypeCashCurrencyMissingInternal: 'Currency missing internally',
      breakTypeCashCurrencyMissingBroker: 'Currency missing on broker',
      breakTypeTradeMissingInternal: 'Trade missing internally',
      breakTypeTradeMissingBroker: 'Trade missing on broker',
      breakTypeTradeQuantity: 'Trade quantity mismatch',
      breakTypeTradePrice: 'Trade price mismatch',
      breakTypeTradeSide: 'Trade side mismatch',
      triggerRunButton: 'Trigger run',
      triggerRunDialogTitle: 'Trigger reconciliation run',
      triggerRunFundIdLabel: 'Fund ID',
      triggerRunFundIdPlaceholder: 'UUID of the fund to reconcile',
      triggerRunUseMockLabel: 'Use mock broker statement (only mode supported today)',
      triggerRunDriftQtyLabel: 'Synthetic position drift (qty)',
      triggerRunDriftCashLabel: 'Synthetic cash drift',
      triggerRunDriftPriceLabel: 'Synthetic trade price drift',
      triggerRunSubmit: 'Submit',
      triggerRunSubmitting: 'Submitting…',
      triggerRunSuccess: 'Run completed.',
      triggerRunError: 'Run failed',
      breakActionAcknowledge: 'Acknowledge',
      breakActionResolve: 'Mark resolved',
      breakActionIgnore: 'Ignore',
      breakActionReopen: 'Re-open',
      breakResolveDialogTitle: 'Resolve break',
      breakResolveNoteLabel: 'Note (recorded on the audit chain)',
      breakResolveSubmit: 'Confirm',
      breakResolveSubmitting: 'Submitting…',
      breakDrillDownTitle: 'Break details',
      breakDetailInternalValue: 'Internal',
      breakDetailBrokerValue: 'Broker',
      breakDetailDiffValue: 'Diff',
      breakDetailDiffPercent: 'Diff %',
      breakDetailDescription: 'Description',
      breakDetailMetadata: 'Metadata',
      drillDownNoBreaks: 'No breaks for this run.',
    },
    surveillance: {
      panelTitle: 'Trade surveillance',
      panelSubtitle:
        'Hourly scan of intraday fills for wash trades, marking-the-close, and self-cross patterns. Hits land on the audit chain — compliance reviews, clears, or escalates from this panel.',
      listEmpty: 'No surveillance events yet.',
      listLoading: 'Loading…',
      listError: 'Failed to load surveillance events',
      refresh: 'Refresh',
      detectedAtLabel: 'Detected',
      ruleCodeLabel: 'Rule',
      severityLabel: 'Severity',
      statusLabel: 'Status',
      symbolLabel: 'Symbol',
      summaryLabel: 'Summary',
      triggerScanButton: 'Trigger scan',
      triggerScanDialogTitle: 'Trigger surveillance scan',
      triggerScanFundIdLabel: 'Fund ID',
      triggerScanFundIdPlaceholder: 'UUID of the fund to scan',
      triggerScanAsOfLabel: 'As-of (YYYY-MM-DD, defaults to today UTC)',
      triggerScanSessionCloseLabel: 'Session close UTC (HH:MM, defaults to 20:00)',
      triggerScanSubmit: 'Run scan',
      triggerScanSubmitting: 'Scanning…',
      triggerScanSuccess: 'Scan completed.',
      triggerScanError: 'Scan failed',
      severityCritical: 'critical',
      severityWarning: 'warning',
      severityInfo: 'info',
      statusOpen: 'open',
      statusReviewing: 'reviewing',
      statusCleared: 'cleared',
      statusEscalated: 'escalated',
      triggerSourceManual: 'manual',
      triggerSourceScheduled: 'scheduled',
      ruleWashTrade: 'Wash trade',
      ruleMarkingClose: 'Marking the close',
      ruleSelfTradePair: 'Self-trade pair',
      ruleRapidFireReversal: 'Rapid-fire reversal',
      ruleLayeringSuspect: 'Layering suspect',
      eventActionAcknowledge: 'Start review',
      eventActionClear: 'Clear',
      eventActionEscalate: 'Escalate',
      eventActionReopen: 'Re-open',
      eventReviewDialogTitle: 'Review surveillance event',
      eventReviewNoteLabel: 'Review note (recorded on the audit chain)',
      eventReviewSubmit: 'Confirm',
      eventReviewSubmitting: 'Submitting…',
      eventDetailMetadata: 'Detection evidence',
      eventDetailTradeIDs: 'Contributing trades',
      eventDetailWindow: 'Detection window',
      runsSubpanelTitle: 'Recent scan runs',
      runsTradeCountLabel: 'Trades scanned',
      runsEventCountLabel: 'Events detected',
      runsDurationLabel: 'Duration',
    },
    drawdown: {
      panelTitle: 'Drawdown soft circuit breaker',
      panelSubtitle:
        'Per-fund tiered DD thresholds evaluated every 5 minutes. Breaches are recorded as proposed trim plans for operator review; tiers flagged auto_execute go straight to the order pipeline (still through risk gates + audit chain).',
      refresh: 'Refresh',
      listEmpty: 'No drawdown events.',
      listLoading: 'Loading…',
      listError: 'Failed to load drawdown data',
      fundIdLabel: 'Fund ID',
      fundIdPlaceholder: 'UUID of the fund to view / configure',
      loadFundButton: 'Load',
      statusTitle: 'Current drawdown status',
      peakNavLabel: 'Peak NAV (lookback)',
      currentNavLabel: 'Current NAV',
      currentDDLabel: 'Current drawdown',
      hasPolicyTrue: 'Policy configured',
      hasPolicyFalse: 'No policy configured',
      breachedTierLabel: 'Tier currently breached',
      triggerCheckButton: 'Run check now',
      triggerCheckRunning: 'Checking…',
      triggerCheckNoBreach: 'No tier breached.',
      triggerCheckBreached: 'Breached:',
      triggerCheckError: 'Check failed',
      tiersTitle: 'Tier configuration (max 5, mildest → hardest)',
      tierLabel: 'Tier',
      ddPctLabel: 'DD threshold (negative; -0.05 = -5%)',
      actionLabel: 'Action',
      trimRatioLabel: 'Trim ratio (used by trim_proportional)',
      cooldownLabel: 'Cooldown (hours)',
      autoExecuteLabel: 'Auto-execute',
      noteLabel: 'Note',
      addTierButton: 'Add / update tier',
      saveTierButton: 'Save',
      saveTierSubmitting: 'Saving…',
      deleteTierButton: 'Delete',
      deleteConfirm: 'Delete this tier?',
      actionTrimProportional: 'Trim proportional',
      actionFlatten: 'Flatten',
      actionDefensiveOnly: 'Defensive only (reject new longs)',
      eventsTitle: 'Drawdown events',
      detectedAtLabel: 'Detected',
      statusLabel: 'Status',
      statusProposed: 'proposed',
      statusApproved: 'approved',
      statusExecuted: 'executed',
      statusDismissed: 'dismissed',
      statusSuperseded: 'superseded',
      trimPlanTitle: 'Trim plan',
      trimPlanEmpty: 'No trim orders for this tier (defensive_only).',
      eventActionApprove: 'Approve & queue orders',
      eventActionDismiss: 'Dismiss',
      eventActionReopen: 'Re-open',
      reviewDialogTitle: 'Review drawdown event',
      reviewNoteLabel: 'Note (recorded on the audit chain)',
      reviewSubmit: 'Confirm',
      reviewSubmitting: 'Submitting…',
      reviewError: 'Failed to update',
    },
    marketStatus: {
      panelTitle: 'Market-status gate (halts / price limits / stale quotes / calendar)',
      panelSubtitle:
        'Pre-trade reachability gate. Suspended or halted instruments, limit-breaching prices, stale quotes, market-closed and half-day-closed sessions are caught BEFORE the matching engine sees the order. Hard rejects block the trade; soft warnings (e.g. mildly stale quote) ride on the order so attribution can see them later.',
      refresh: 'Refresh',
      instrumentsTitle: 'Instrument status',
      instrumentsEmpty: 'No instruments configured yet.',
      fieldKey: 'instrument_key',
      fieldSymbol: 'Symbol',
      fieldMarket: 'Market',
      fieldStatus: 'Status',
      fieldHaltReason: 'Halt reason',
      fieldHaltUntil: 'Halt until',
      fieldLower: 'Lower limit',
      fieldUpper: 'Upper limit',
      fieldLastQuoteAt: 'Last quote',
      fieldStalenessBudget: 'Staleness budget (s)',
      statusTrading: 'Trading',
      statusHalted: 'Halted',
      statusSuspended: 'Suspended',
      haltButton: 'Halt',
      haltSubmitting: 'Submitting…',
      haltDialogTitle: 'Halt instrument',
      haltReasonLabel: 'Reason (required)',
      haltUntilLabel: 'Halt until (optional RFC3339)',
      unhaltButton: 'Unhalt',
      setLimitsButton: 'Set price limits',
      setLimitsDialogTitle: 'Price limits',
      upsertDialogTitle: 'Edit instrument',
      saveButton: 'Save',
      saveSubmitting: 'Saving…',
      cancelButton: 'Cancel',
      eventsTitle: 'Gate events',
      eventDecision: 'Decision',
      eventRule: 'Rule',
      eventSummary: 'Summary',
      eventDetected: 'When',
      decisionAllow: 'allow',
      decisionWarn: 'warn',
      decisionReject: 'reject',
      ruleHalted: 'halted',
      ruleSuspended: 'suspended',
      rulePriceLimit: 'price-limit',
      ruleStaleQuote: 'stale quote',
      ruleMarketClosed: 'market closed',
      ruleHalfDayClosed: 'half-day closed',
      calendarTitle: 'Trading calendar',
      calendarMarketLabel: 'Market (e.g. CN / US / HK)',
      calendarFromLabel: 'From',
      calendarToLabel: 'To',
      calendarLoadButton: 'Load',
      calendarUpsertTitle: 'Add / edit calendar day',
      calendarIsOpen: 'Open',
      calendarHalfDay: 'Half-day',
      calendarOpenLocal: 'Open local (HH:MM)',
      calendarCloseLocal: 'Close local (HH:MM)',
      calendarTZ: 'Timezone',
      calendarNote: 'Note',
      error: 'Failed to load',
    },
    marketImpact: {
      panelTitle: 'S6.2 · Market-impact calibration',
      panelSubtitle:
        "Per-instrument ADV and volatility used by the simulator's square-root impact model (bps = σ · k · √(Q/ADV) · 10000). Uncalibrated names fall back to asset-class defaults so big orders never silently fill at last.",
      refresh: 'Refresh',
      instrumentsTitle: 'Calibration rows',
      instrumentsEmpty: 'No calibrations yet. Add one below or leave empty to use asset-class defaults.',
      fieldKey: 'instrument_key',
      fieldSymbol: 'Symbol',
      fieldMarket: 'Market',
      fieldAssetClass: 'Asset class',
      fieldADV: 'ADV (shares / contracts)',
      fieldADVNotional: 'ADV (notional)',
      fieldVolatility: 'Daily volatility σ',
      fieldImpactCoef: 'Impact coef k',
      fieldImpactExp: 'Exponent α (default 0.5)',
      fieldMinBps: 'Min slippage bps',
      fieldMaxBps: 'Max slippage bps',
      fieldLastCalibrated: 'Last calibrated',
      fieldSource: 'Source',
      upsertButton: 'Upsert',
      upsertDialogTitle: 'Edit calibration',
      deleteButton: 'Delete',
      deleteConfirm: 'Delete this calibration? Future fills will fall back to asset-class defaults.',
      saveButton: 'Save',
      saveSubmitting: 'Saving…',
      cancelButton: 'Cancel',
      sourceManual: 'Manual entry',
      sourceHistorical: 'Historical replay',
      sourceBrokerReported: 'Broker-reported',
      previewTitle: 'Preview impact',
      previewSubtitle: 'Run the engine on a probe; nothing is booked. Useful for sanity-checking calibration.',
      previewSide: 'Side',
      previewSideBuy: 'Buy',
      previewSideSell: 'Sell',
      previewQuantity: 'Quantity',
      previewReferencePrice: 'Reference price',
      previewSubmit: 'Run preview',
      previewSubmitting: 'Running…',
      previewResult: 'Preview result',
      previewBps: 'Adverse bps',
      previewImpliedFill: 'Implied fill',
      previewImpactCost: 'Impact cost (notional)',
      previewUsedDefaults: 'Asset-class default',
      previewUsedADVFallback: 'ADV missing → floor only',
      cacheTitle: 'In-memory cache',
      cacheSize: 'Calibration rows',
      cacheLastRefresh: 'Last refresh',
      cacheRefreshButton: 'Force refresh',
      cacheRefreshing: 'Refreshing…',
      error: 'Failed to load',
    },
    lockup: {
      panelTitle: 'S6.3 · IPO / restricted-share lock-up',
      panelSubtitle:
        "Records hold-periods on positions acquired via IPO allocation, private placement, RSU vest, or other restricted channels. The simulator rejects sells whose qty exceeds (position − sum of active locked qty). Operators can early-release with an audited reason.",
      refresh: 'Refresh',
      listTitle: 'Lock-up records',
      listEmpty: 'No lock-up records yet',
      fieldFund: 'Fund',
      fieldInstrument: 'instrument_key',
      fieldSymbol: 'Symbol',
      fieldQty: 'Locked qty',
      fieldUntil: 'Locked until',
      fieldReason: 'Reason',
      fieldNote: 'Note',
      fieldStatus: 'Status',
      fieldSourceLot: 'Source lot',
      fieldReleasedAt: 'Released at',
      fieldReleasedReason: 'Released reason',
      statusActive: 'Active',
      statusExpired: 'Expired',
      statusReleased: 'Released',
      reasonIPO: 'IPO allocation',
      reasonPrivatePlacement: 'Private placement',
      reasonRSU: 'RSU vest',
      reasonRestricted: 'Restricted',
      reasonEmployeeGrant: 'Employee grant',
      reasonBlockSale: 'Block sale',
      reasonOther: 'Other',
      filterAll: 'All',
      createButton: 'Add',
      createDialogTitle: 'Record lock-up',
      editButton: 'Edit',
      editDialogTitle: 'Edit lock-up',
      deleteButton: 'Delete',
      deleteConfirm:
        'Hard-deleting loses the audit trail. Are you sure you want to delete instead of "early release"?',
      releaseButton: 'Early release',
      releaseDialogTitle: 'Release lock-up early',
      releaseReasonLabel: 'Release reason (required, will be audit-logged)',
      saveButton: 'Save',
      saveSubmitting: 'Saving…',
      cancelButton: 'Cancel',
      error: 'Failed to load',
    },
    borrow: {
      panelTitle: 'S6.4 · Securities borrow & locate',
      panelSubtitle:
        'Records the annual borrow rate, locate fee and available supply per borrowable instrument. The simulator runs a pre-trade locate gate on SHORT opens; an EOD loop accrues short-borrow fees (qty × close × rate / 365) to the cash_ledger (borrow_fee) and the short-borrow sub-ledger.',
      refresh: 'Refresh',
      listTitle: 'Borrow rates',
      listEmpty: 'No borrow rate calibrations yet',
      fieldKey: 'instrument_key',
      fieldSymbol: 'Symbol',
      fieldMarket: 'Market',
      fieldRate: 'Annual rate (bps)',
      fieldLocateFee: 'Locate fee (bps)',
      fieldAvailability: 'Availability',
      fieldAvailable: 'Available shares',
      fieldMinLocate: 'Min locate qty',
      fieldMaxLocate: 'Max locate qty',
      fieldSource: 'Source',
      fieldNote: 'Note',
      availEasy: 'Easy-to-borrow',
      availHard: 'Hard-to-borrow',
      availRestricted: 'Restricted',
      availUnavailable: 'Unavailable',
      sourceManual: 'Manual',
      sourceBrokerQuote: 'Broker quote',
      sourceAgentLender: 'Agent lender',
      sourceHistorical: 'Historical calibration',
      sourcePublicFeed: 'Public feed',
      upsertButton: 'Save',
      upsertSubmitting: 'Saving…',
      deleteButton: 'Delete',
      cacheTitle: 'In-memory cache',
      cacheSize: 'Rows',
      cacheLastRefresh: 'Last refresh',
      cacheRefreshButton: 'Force refresh',
      cacheRefreshing: 'Refreshing…',
      previewTitle: 'Locate preview',
      previewSubtitle:
        'Dry-run the locate gate decision (including locate fee) without placing an order.',
      previewFundLabel: 'Fund ID',
      previewKeyLabel: 'instrument_key',
      previewQtyLabel: 'Requested qty',
      previewPriceLabel: 'Intended price',
      previewSubmit: 'Preview',
      previewSubmitting: 'Computing…',
      previewResultDecision: 'Decision',
      previewResultRate: 'Annual borrow rate (bps)',
      previewResultLocateFee: 'Locate fee',
      previewResultNotional: 'Notional',
      auditTitle: 'Locate audit log',
      auditFundFilter: 'Filter by fund',
      auditDecisionFilter: 'Filter by decision',
      auditEmpty: 'No locate events recorded',
      ledgerTitle: 'Borrow-fee ledger',
      ledgerEmpty: 'No borrow-fee accruals yet',
      error: 'Failed to load',
    },
    wsfeed: {
      panelTitle: 'S6.5 · Real-time market data (WS)',
      panelSubtitle:
        'Replace REST polling on the broker / position-refresh hot paths with pushed ticks. Set WSFEED_ENABLED=true plus a provider; cache misses fall back to REST transparently.',
      disabled: 'Currently disabled: ',
      refresh: 'Refresh',
      reconcile: 'Reconcile subscriptions',
      reconcileSubmitting: 'Reconciling…',
      statusEnabled: 'Running',
      statusHealthyProviders: 'Healthy providers',
      statusSubscriptions: 'Subscriptions',
      statusCacheSymbols: 'Cached symbols',
      statusTotalTicks: 'Total ticks',
      statusDroppedEvents: 'Dropped events',
      connectionsTitle: 'Upstream connections',
      connectionsEmpty: 'No providers registered',
      colProvider: 'Provider',
      colState: 'State',
      colTickCount: 'Ticks',
      colReconnects: 'Reconnects',
      colLastTick: 'Last tick',
      colConnectedAt: 'Connected at',
      colLastError: 'Last error',
      stateConnected: 'connected',
      stateConnecting: 'connecting',
      stateReconnecting: 'reconnecting',
      stateBackoff: 'backoff',
      stateDisconnected: 'disconnected',
      stateClosed: 'closed',
      stateUnknown: 'unknown',
      subscriptionsTitle: 'Active subscriptions',
      subscriptionsEmpty: 'No active subscriptions',
      colSymbol: 'Symbol',
      colMarket: 'Market',
      colConsumers: 'Consumers',
      cacheTitle: 'Quote cache',
      cacheStats: 'Hits / Misses / Stale / Evicts',
      cacheEmpty: 'Cache is empty',
      colLast: 'Last',
      colBid: 'Bid',
      colAsk: 'Ask',
      colAsOf: 'As of',
      colStale: 'Stale',
      subscribeTitle: 'Manual subscribe',
      subscribeSymbolPlaceholder: 'Symbol (e.g. AAPL)',
      subscribeMarketPlaceholder: 'Market (e.g. US)',
      subscribeSubmit: 'Subscribe',
      subscribeSubmitting: 'Submitting…',
      unsubscribeButton: 'Unsubscribe',
      evictCacheTitle: 'Cache maintenance',
      evictCacheButton: 'Evict row',
      evictCacheAllButton: 'Evict all',
      error: 'Failed to load',
    },
    factorExposure: {
      panelTitle: 'Factor exposure',
      panelSubtitle: 'Net and gross exposure of the current portfolio across the six canonical factors (size / value / momentum / quality / lowvol / market_beta), with coverage of the holdings the read was based on.',
      refresh: 'Refresh',
      loading: 'Computing…',
      empty: 'No active holdings',
      error: 'Failed to load',
      navLabel: 'Gross MV',
      holdingsLabel: 'Holdings',
      coverageLabel: 'Coverage',
      loadingsAsOfLabel: 'Loadings as of',
      loadingsAsOfStale: 'Stale calibration',
      factorSize: 'Size',
      factorValue: 'Value',
      factorMomentum: 'Momentum',
      factorQuality: 'Quality',
      factorLowVol: 'Low Vol',
      factorMarketBeta: 'Market β',
      netExposureLabel: 'Net',
      grossExposureLabel: 'Gross',
      holdingCountLabel: 'Contributing holdings',
      trendTitle: '30-day trend',
      trendEmpty: 'No historical snapshot yet',
      adminPanelTitle: 'Factor loading store',
      adminPanelSubtitle: 'Manage the instrument_factor_loadings table. Sources manual / msci / eastmoney / computed / override correspond to manual entry, third-party vendor data, Quant Lab batch output, and emergency overrides.',
      adminListTitle: 'Current calibrations',
      adminListEmpty: 'No calibrations on file',
      adminInstrumentKey: 'Instrument key',
      adminFactorLabel: 'Factor',
      adminAsOfLabel: 'Asof',
      adminLoadingLabel: 'Loading',
      adminSourceLabel: 'Source',
      adminNoteLabel: 'Note',
      adminUpdatedAtLabel: 'Updated',
      adminFactorAll: 'All factors',
      adminUpsertTitle: 'Add / update loading',
      adminUpsertSubmit: 'Save',
      adminUpsertSubmitting: 'Saving…',
      adminDeleteButton: 'Delete',
      adminDeleteConfirm: 'Delete this calibration row?',
      sourceManual: 'Manual',
      sourceEastMoney: 'EastMoney',
      sourceMSCI: 'MSCI',
      sourceComputed: 'Quant Lab',
      sourceOverride: 'Override',
    },
    varRisk: {
      panelTitle: 'Value at Risk (VaR / CVaR)',
      panelSubtitle: 'One-period worst-case loss estimated from nav_snapshots.daily_return using three methods (historical / parametric / Monte Carlo) and three confidence levels (90% / 95% / 99%).',
      refresh: 'Refresh',
      loading: 'Computing…',
      empty: 'No data',
      error: 'Failed to load',
      insufficientHistory: 'Not enough history yet — accumulate at least 20 trading days of NAV first.',
      sampleSizeLabel: 'Sample',
      lookbackLabel: 'Lookback',
      horizonLabel: 'Horizon',
      horizon1d: '1 day',
      horizon5d: '5 days',
      horizon10d: '10 days',
      meanLabel: 'Mean daily return',
      stdevLabel: 'Volatility',
      sampleWindowLabel: 'Sample window',
      methodLabel: 'Method',
      confidenceLabel: 'Confidence',
      varLabel: 'VaR',
      cvarLabel: 'CVaR',
      methodHistorical: 'Historical',
      methodParametric: 'Parametric',
      methodMonteCarlo: 'Monte Carlo',
      methodHistoricalSubtitle: 'Non-parametric percentile of realised returns',
      methodParametricSubtitle: 'Normal closed-form μ − z·σ',
      methodMonteCarloSubtitle: '50 000 draws from N(μ, σ), empirical percentile',
      confidence90Label: '90%',
      confidence95Label: '95%',
      confidence99Label: '99%',
      varInterpretation: 'Loss is expected to stay above this number with the given confidence over one horizon',
      cvarInterpretation: 'Expected loss conditional on VaR being breached (tail expectation)',
    },
    stressTest: {
      panelTitle: 'Stress test',
      panelSubtitle: 'Pick a scenario from the admin-curated library (historical / hypothetical / regulatory) and project its shocks against the current portfolio. NAV impact appears below.',
      runButton: 'Run scenario',
      running: 'Running…',
      refresh: 'Refresh',
      empty: 'Pick a scenario to see its impact',
      error: 'Run failed',
      scenarioLabel: 'Scenario',
      scenarioPlaceholder: 'Choose a stress scenario…',
      categoryLabel: 'Category',
      descriptionLabel: 'Description',
      shockCountLabel: 'Shocks',
      navBeforeLabel: 'NAV before',
      navAfterLabel: 'NAV after',
      pnlTotalLabel: 'PnL',
      pnlPctLabel: 'PnL %',
      holdingsLabel: 'Holdings',
      shockedLabel: 'Shocked holdings',
      impactsTitle: 'Per-holding impact',
      impactsEmpty: 'No holding-level impact yet',
      impactSymbol: 'Symbol',
      impactBefore: 'Before',
      impactAfter: 'After',
      impactPnL: 'PnL',
      impactReturn: 'Applied return',
      impactShock: 'Matched shock',
      categoryHistorical: 'Historical',
      categoryHypothetical: 'Hypothetical',
      categoryRegulatory: 'Regulatory',
      adminPanelTitle: 'Stress scenario library',
      adminPanelSubtitle: 'Maintain the stress_scenarios table. Shocks match holdings by specificity (instrument > market > asset_class > factor > wildcard); factor shocks combine additively with instrument_factor_loadings.',
      adminListTitle: 'Current scenarios',
      adminListEmpty: 'No scenarios on file — add the first one',
      adminScenarioName: 'Name',
      adminScenarioCategory: 'Category',
      adminScenarioDescription: 'Description',
      adminScenarioShocks: 'Shocks',
      adminScenarioCreatedBy: 'Author',
      adminScenarioUpdatedAt: 'Updated',
      adminUpsertTitle: 'Add / update scenario',
      adminUpsertSubmit: 'Save',
      adminUpsertSubmitting: 'Saving…',
      adminDeleteButton: 'Delete',
      adminDeleteConfirm: 'Delete this scenario? Historical stress results will be cascade-deleted.',
      targetInstrument: 'Instrument',
      targetMarket: 'Market',
      targetAssetClass: 'Asset class',
      targetFactor: 'Factor',
      targetWildcard: 'Wildcard (all holdings)',
    },
    brinsonAttribution: {
      panelTitle: 'Brinson Attribution',
      panelSubtitle: 'Decompose active return (portfolio − benchmark) into allocation, selection and interaction effects per bucket. Admin maintains the benchmark composition; the fund-level runner derives the portfolio side from live holdings.',
      runButton: 'Run attribution',
      running: 'Running…',
      benchmarkLabel: 'Benchmark',
      benchmarkPlaceholder: 'Pick a benchmark…',
      dimensionLabel: 'Dimension',
      dimensionAssetClass: 'Asset class',
      dimensionMarket: 'Market',
      dimensionSector: 'Sector (not yet supported)',
      benchmarkEmpty: 'No benchmark compositions available — ask an admin to seed brinson_benchmark_compositions',
      portfolioReturn: 'Portfolio return',
      benchmarkReturn: 'Benchmark return',
      activeReturn: 'Active return',
      allocationEffect: 'Allocation effect',
      selectionEffect: 'Selection effect',
      interactionEffect: 'Interaction effect',
      totalEffect: 'Total effect',
      decompositionTitle: 'Three-effect decomposition',
      bucketsTitle: 'Per-bucket detail',
      bucketsEmpty: 'No bucket detail',
      colBucket: 'Bucket',
      colPortfolioWeight: 'Port. wt',
      colBenchmarkWeight: 'Bench. wt',
      colPortfolioReturn: 'Port. ret',
      colBenchmarkReturn: 'Bench. ret',
      colAllocation: 'Allocation',
      colSelection: 'Selection',
      colInteraction: 'Interaction',
      colTotal: 'Total',
      persistLabel: 'Archive this run',
      error: 'Attribution failed',
      noPortfolioHoldings: 'No holdings carry the requested dimension',
      compositionNotFound: 'Composition not found for the chosen benchmark',
      sectorUnsupported: 'Sector dimension requires holding-level sector classification (not yet wired)',
      asofLabel: 'As of',
      adminPanelTitle: 'Brinson benchmark compositions',
      adminPanelSubtitle: 'Maintain brinson_benchmark_compositions. One row per (benchmark_id, dimension, asof). Buckets JSONB array holds {key, weight, return_pct} where weight is a fraction (sum ≈ 1).',
      adminListTitle: 'Current compositions',
      adminListEmpty: 'No compositions yet',
      adminUpsertTitle: 'Add / update composition',
      adminUpsertSubmit: 'Save',
      adminUpsertSubmitting: 'Saving…',
      adminDeleteButton: 'Delete',
      adminDeleteConfirm: 'Delete this composition? Archived attribution snapshots that reference it will be cascade-deleted.',
      adminBucketKey: 'Bucket key',
      adminBucketWeight: 'Weight (0–1)',
      adminBucketReturn: 'Return (e.g. 0.05)',
      adminAddBucket: '+ Add bucket',
      adminRemoveBucket: 'Remove',
      adminBenchmarkId: 'Benchmark ID',
      adminAsof: 'As-of date',
      adminNote: 'Note',
    },
    analystPanel: {
      title: 'Analyst Panel',
      subtitle: 'Four specialised analysts (fundamentals / sentiment / news / technical) each produce an independent verdict; the panel blends them by confidence weight. Every analyst is anchored to a deterministic rule; the LLM only fills in the narrative on top.',
      symbolLabel: 'Symbol',
      symbolPlaceholder: 'e.g. AAPL / 600519',
      runButton: 'Run analyst panel',
      running: 'Polling 4 analysts in parallel…',
      persistLabel: 'Archive this panel',
      aggregateTitle: 'Aggregate verdict',
      aggregateDirection: 'Direction',
      aggregateConfidence: 'Confidence',
      categoriesVoted: 'Analysts that voted',
      voteSummary: '{voted} of {total} analysts took a side',
      perCategoryTitle: 'Per-analyst reports',
      asof: 'As of',
      generatedAt: 'Generated at',
      directionBullish: 'Bullish',
      directionBearish: 'Bearish',
      directionNeutral: 'Neutral',
      categoryFundamentals: 'Fundamentals',
      categorySentiment: 'Sentiment',
      categoryNews: 'News / catalysts',
      categoryTechnical: 'Technical',
      thesisLabel: 'Thesis',
      keyFindingsLabel: 'Key findings',
      risksLabel: 'Risks',
      dataPointsLabel: 'Data points',
      sourcesLabel: 'Sources',
      noPanelYet: 'No panel run yet',
      error: 'Panel run failed',
      historyTitle: 'Historical panels',
      historyEmpty: 'No historical panels yet',
      historyLoading: 'Loading…',
      confidenceLabel: 'confidence {value}%',
      llmModelFallback: 'rule fallback',
      llmModelLLM: 'LLM',
    },
    bullBearDebate: {
      title: 'Bull / Bear Debate',
      subtitle: 'Two forced personas — Bull and Bear — argue against each other over the analyst panel\'s conclusions. Bull must find the strongest reason to buy; Bear must find the strongest reason to sell or avoid. Neither is allowed to settle on neutral. Later rounds carry more weight in the final verdict the PM reads.',
      symbolLabel: 'Symbol',
      symbolPlaceholder: 'e.g. AAPL / 600519',
      roundsLabel: 'Debate rounds',
      runButton: 'Run debate',
      running: 'Researchers debating…',
      notesLabel: 'Notes',
      verdictTitle: 'Verdict',
      verdictDirection: 'Direction',
      verdictConfidence: 'Confidence',
      verdictContested: 'Contested (margin < 20%)',
      verdictNotContested: 'Decisive margin',
      bullConfidence: 'Bull avg confidence',
      bearConfidence: 'Bear avg confidence',
      winnerBull: 'Bull wins',
      winnerBear: 'Bear wins',
      winnerTie: 'Tie',
      argumentsTitle: 'Per-round arguments',
      roundLabel: 'Round {round}',
      stanceBull: 'Bull',
      stanceBear: 'Bear',
      thesisLabel: 'Thesis',
      supportPointsLabel: 'Support points',
      rebuttalsLabel: 'Rebuttals',
      citedReportsLabel: 'Cited analysts',
      noDebateYet: 'No debate run yet',
      error: 'Debate run failed',
      historyTitle: 'Historical debates',
      historyEmpty: 'No historical debates yet',
      historyLoading: 'Loading…',
      confidenceLabel: 'confidence {value}%',
      llmModelFallback: 'rule fallback',
      llmModelLLM: 'LLM',
    },
    corpActions: {
      title: 'Dividends · Splits · Rights Issues',
      subtitle: 'Recent corporate actions applied to this fund',
      expand: 'Show',
      collapse: 'Hide',
      loading: 'Loading…',
      error: 'Failed to load',
      retry: 'Retry',
      empty: 'No recent corporate actions',
      typeSplit: 'Split / Stock div.',
      typeCashDividend: 'Cash dividend',
      typeStockDividend: 'Stock dividend',
      typeCombined: 'Stock + cash',
      sharesLabel: 'Shares',
      costLabel: 'Cost',
      cashLabel: 'Cash credit',
      exDateLabel: 'Ex-date',
    },
    benchmark: {
      title: 'Fund vs Market',
      subtitle: 'Fund NAV and benchmarks rebased to 100 at start',
      fund: 'This fund',
      days7: '7d',
      days30: '30d',
      days90: '90d',
      days180: '180d',
      days365: '1y',
      expand: 'Show',
      collapse: 'Hide',
      loading: 'Loading…',
      empty: 'No NAV history yet',
      error: 'Failed to load benchmarks',
      retry: 'Retry',
      seriesPicker: 'Benchmarks',
      addSeries: 'Add benchmark',
      partialFailureToast: 'Some benchmarks could not be loaded',
      legendStart: 'start = 100',
      holdingOverlapDominantTitle: 'Your fund ≈ this benchmark',
      holdingOverlapDominantBody: 'The fund\'s largest position is the same instrument as the selected benchmark, so the two lines in Compare mode track each other closely. Switch to the Alpha view to see relative outperformance.',
      holdingOverlapPartialTitle: 'Holdings overlap the benchmark',
      holdingOverlapPartialBody: 'Some of the fund\'s holdings overlap the selected benchmark, which can make the Compare view harder to read. Switch to Alpha for relative performance.',
      holdingOverlapSwitchToAlpha: 'Switch to Alpha view',
    },
    holdingsSeries: {
      title: 'Holdings trends',
      subtitle: 'Per-holding normalized price (start = 100)',
      expand: 'Show',
      collapse: 'Hide',
      loading: 'Loading…',
      error: 'Failed to load trends',
      retry: 'Retry',
      empty: 'No holdings to plot',
      vsEntry: 'vs entry',
      vsStart: 'vs window start',
      partialFailureToast: 'Holdings that couldn\'t be loaded',
      days30: '30d',
      days90: '90d',
      days180: '180d',
    },
    abShadow: {
      sectionTitle: 'Shadow agent comparison',
      sectionSubtitle: 'See what each variant\u2019s agents learned during shadow execution \u2014 lessons, adjustments, and proposed evolution-config changes.',
      expand: 'Show',
      collapse: 'Hide',
      loading: 'Loading shadow agents\u2026',
      error: 'Failed to load shadow agents',
      retry: 'Retry',
      empty: 'No shadow learning data for this test yet',
      notAnalyzedYet: 'Run \u201cGenerate analysis\u201d first to compare A vs B shadow agent learning.',
      columnA: 'Variant A',
      columnB: 'Variant B',
      eventCount: 'Learning events',
      latestDate: 'Latest event',
      lessons: 'Lessons',
      adjustments: 'Adjustments',
      summaries: 'Recent summaries',
      timeline: 'Daily timeline',
      memories: 'Shadow memories',
      proposedDiff: 'Proposed evolution_config change',
      diffAdded: 'Added',
      diffChanged: 'Changed (prev \u2192 new)',
      diffRemoved: 'Removed',
      noDiff: 'No change vs current evolution_config',
      deterministicShadowBanner: 'Variant B currently uses deterministic shadow execution; numbers are sanity-check only. Card K will introduce real LLM shadow runs.',
    },
    abAttribution: {
      sectionTitle: 'Per-symbol attribution',
      sectionSubtitle: 'Compare A vs B trade count, turnover, and realized P&L per symbol.',
      expand: 'Show',
      collapse: 'Hide',
      loading: 'Loading attribution\u2026',
      error: 'Failed to load attribution',
      retry: 'Retry',
      empty: 'No shadow trade attribution for this test yet',
      columnSymbol: 'Symbol',
      columnTradesA: 'A trades',
      columnTradesB: 'B trades',
      columnPnLA: 'A realized P&L',
      columnPnLB: 'B realized P&L',
      columnTurnoverA: 'A turnover',
      columnTurnoverB: 'B turnover',
      columnGap: 'Gap (B \u2212 A)',
      columnGapPct: 'Gap % of turnover',
      columnWinner: 'Winner',
      winnerA: 'A',
      winnerB: 'B',
      winnerTie: 'Tie',
      totalsTitle: 'Totals',
      avgPnL: 'Avg P&L',
      winRate: 'Winning trade rate',
    },
  },
};

/**
 * resolveMessage — pure helper to look up a dot path like "auth.email".
 * Returns the fallback (== same path) when missing so the UI shows the
 * path rather than crashing — easier to debug.
 */
export function resolveMessage(locale: LocaleId, path: string): string {
  const bundle = messages[locale] ?? messages['zh-CN'];
  const segments = path.split('.');
  let cursor: unknown = bundle;
  for (const seg of segments) {
    if (cursor && typeof cursor === 'object' && seg in (cursor as Record<string, unknown>)) {
      cursor = (cursor as Record<string, unknown>)[seg];
    } else {
      return path;
    }
  }
  return typeof cursor === 'string' ? cursor : path;
}
