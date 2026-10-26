import adminAccessKeys from './adminAccessKeys'
import landing from './landing'
import common from './common'
import dashboard from './dashboard'
import channelMonitorV2 from './channelMonitorV2'
import admin from './admin'
import misc from './misc'

export default {
  adminAccessKeys,
  ...landing,
  ...common,
  ...dashboard,
  ...channelMonitorV2,
  admin,
  ...misc,
}
