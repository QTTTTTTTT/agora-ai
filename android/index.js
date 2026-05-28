/**
 * @format
 * RN app entry. Registered with AppRegistry so the native Android shell
 * (still to be initialised via `npx react-native init` per
 * docs/ANDROID_BOOTSTRAP.md) can mount App.
 */

import { AppRegistry } from 'react-native';
import App from './src/App';
import { name as appName } from './app.json';

AppRegistry.registerComponent(appName, () => App);
