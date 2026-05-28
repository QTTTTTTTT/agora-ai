const { getDefaultConfig, mergeConfig } = require('@react-native/metro-config');
const path = require('path');

// Sprint 3 / Android skeleton: 让 metro 能解析 npm workspace 里的
// @fundai/api-client。默认 metro 只看本包 node_modules；这里把
// 仓库根目录也加进 watchFolders + nodeModulesPaths。
const workspaceRoot = path.resolve(__dirname, '..');
const projectRoot = __dirname;

const config = {
  watchFolders: [workspaceRoot],
  resolver: {
    nodeModulesPaths: [
      path.resolve(projectRoot, 'node_modules'),
      path.resolve(workspaceRoot, 'node_modules'),
    ],
    disableHierarchicalLookup: false,
  },
};

module.exports = mergeConfig(getDefaultConfig(__dirname), config);
