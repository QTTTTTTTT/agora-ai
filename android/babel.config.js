module.exports = {
  presets: ['module:@react-native/babel-preset'],
  plugins: [
    // Reanimated 必须放最后
    'react-native-reanimated/plugin',
  ],
};
